package usecase

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_cepUsecaseOnce sync.Once
	_cepUsecaseObj  *CEPUsecase
)

var _ model.ProcessorInterface = (*CEPUsecase)(nil)

// CEPUsecase evaluates incoming events against a set of CEP patterns.
type CEPUsecase struct {
	store    model.ProgressStore
	ruleUC   ruleModel.RuleUsecaseInterface
	patterns []model.CEPPattern
}

func NewCEPUsecase(store model.ProgressStore, ruleUC ruleModel.RuleUsecaseInterface) *CEPUsecase {
	_cepUsecaseOnce.Do(func() {
		_cepUsecaseObj = &CEPUsecase{store: store, ruleUC: ruleUC}
	})
	return _cepUsecaseObj
}

// NewCEPUsecaseWith creates a non-singleton instance (for testing with alternative connections).
func NewCEPUsecaseWith(store model.ProgressStore, ruleUC ruleModel.RuleUsecaseInterface) *CEPUsecase {
	return &CEPUsecase{store: store, ruleUC: ruleUC}
}

// AddPattern registers a pattern for evaluation.
func (p *CEPUsecase) AddPattern(pattern model.CEPPattern) {
	p.patterns = append(p.patterns, pattern)
}

// ProcessEvent evaluates the event against all registered patterns and returns any completed matches.
//
// Processing order:
//  1. Advance all in-progress pattern instances that are waiting for the next state.
//  2. Attempt to start new instances by matching the first state of every pattern.
func (p *CEPUsecase) ProcessEvent(ctx context.Context, event *model.Event) ([]*model.MatchResult, error) {
	inProgress, err := p.store.ListByMember(ctx, event.MemberID)
	if err != nil {
		return nil, fmt.Errorf("process event: list in-progress: %w", err)
	}

	var results []*model.MatchResult

	// Step 1: advance existing in-progress instances.
	for _, progress := range inProgress {
		pattern, ok := p.findPattern(progress.PatternID)
		if !ok {
			continue
		}

		result, err := p.advanceProgress(ctx, event, pattern, progress)
		if err != nil {
			return nil, fmt.Errorf("process event: advance pattern %s: %w", pattern.ID, err)
		}
		if result != nil {
			results = append(results, result)
		}
	}

	// Step 2: try to start new pattern instances from state[0].
	for _, pattern := range p.patterns {
		if len(pattern.States) == 0 {
			continue
		}
		matched, err := p.matchState(event, pattern.States[0], nil)
		if err != nil {
			return nil, fmt.Errorf("process event: start pattern %s state 0: %w", pattern.ID, err)
		}
		if !matched {
			continue
		}

		vars := extractBindings(event, pattern.States[0].ContextBinding)
		now := time.Now()
		progress := &model.PatternProgress{
			ID:              uuid.NewString(),
			PatternID:       pattern.ID,
			MemberID:        event.MemberID,
			CurrentStep:     1, // already matched step 0
			Variables:       vars,
			StartedAt:       now,
			ExpiresAt:       patternExpiry(pattern, now),
			ProcessedEvents: []string{event.EventID},
		}

		// A single-state pattern is an instant match.
		if len(pattern.States) == 1 {
			results = append(results, &model.MatchResult{
				PatternID:   pattern.ID,
				PatternName: pattern.Name,
				MemberID:    event.MemberID,
				Variables:   vars,
				MatchedAt:   now,
			})
			continue
		}

		if err := p.store.Save(ctx, progress); err != nil {
			return nil, fmt.Errorf("process event: save new progress for pattern %s: %w", pattern.ID, err)
		}
	}

	return results, nil
}

// advanceProgress checks whether the event satisfies the next expected state for a progress record.
// Returns a MatchResult if this event completes the pattern; returns nil otherwise.
func (p *CEPUsecase) advanceProgress(ctx context.Context, event *model.Event, pattern model.CEPPattern, progress *model.PatternProgress) (*model.MatchResult, error) {
	if progress.CurrentStep >= len(pattern.States) {
		return nil, nil
	}

	// Idempotency: skip if this event was already processed by this progress instance.
	if slices.Contains(progress.ProcessedEvents, event.EventID) {
		return nil, nil
	}

	state := pattern.States[progress.CurrentStep]

	// Check MaxWait expiry for this step.
	if state.MaxWait != nil {
		deadline := progress.StartedAt.Add(windowDuration(state.MaxWait))
		if event.OccurredAt.After(deadline) {
			// Window expired; discard this progress instance.
			if err := p.store.Delete(ctx, progress.ID); err != nil {
				return nil, fmt.Errorf("delete expired progress %s: %w", progress.ID, err)
			}
			return nil, nil
		}
	}

	matched, err := p.matchState(event, state, progress.Variables)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, nil
	}

	// Merge newly extracted bindings into accumulated variables.
	newVars := extractBindings(event, state.ContextBinding)
	for k, v := range newVars {
		progress.Variables[k] = v
	}
	progress.CurrentStep++
	progress.ProcessedEvents = append(progress.ProcessedEvents, event.EventID)

	if progress.CurrentStep >= len(pattern.States) {
		// Pattern fully matched.
		result := &model.MatchResult{
			PatternID:   pattern.ID,
			PatternName: pattern.Name,
			MemberID:    event.MemberID,
			Variables:   progress.Variables,
			MatchedAt:   event.OccurredAt,
		}
		if err := p.store.Delete(ctx, progress.ID); err != nil {
			return nil, fmt.Errorf("delete completed progress %s: %w", progress.ID, err)
		}
		return result, nil
	}

	if err := p.store.Save(ctx, progress); err != nil {
		return nil, fmt.Errorf("save advanced progress %s: %w", progress.ID, err)
	}
	return nil, nil
}

// matchState evaluates the state's condition against the event, injecting any accumulated variables.
func (p *CEPUsecase) matchState(event *model.Event, state model.PatternState, vars map[string]any) (bool, error) {
	fields := make(map[string]any, len(event.Fields)+3)
	for k, v := range event.Fields {
		fields[k] = v
	}
	fields["behavior"] = event.Behavior
	fields["member_id"] = event.MemberID


	evalCtx := ruleModel.NewMapContext(fields)
	if len(vars) > 0 {
		evalCtx = evalCtx.WithVariables(vars)
	}

	ok, err := p.ruleUC.Evaluate(state.Condition, evalCtx)
	if err != nil {
		return false, fmt.Errorf("match state %q: %w", state.Name, err)
	}
	return ok, nil
}

// extractBindings resolves context bindings from the event.
// Binding values use the syntax "$event.field_path" — the "$event." prefix is stripped
// and the remainder is looked up in event.Fields.
func extractBindings(event *model.Event, bindings map[string]string) map[string]any {
	result := make(map[string]any, len(bindings))
	for varName, src := range bindings {
		fieldPath := strings.TrimPrefix(src, "$event.")
		if v, ok := event.Fields[fieldPath]; ok {
			result[varName] = v
		}
	}
	return result
}

// patternExpiry computes a generous TTL for the progress record based on all MaxWait windows.
func patternExpiry(pattern model.CEPPattern, from time.Time) time.Time {
	expiry := from.Add(24 * time.Hour) // default safety
	for _, s := range pattern.States {
		if s.MaxWait != nil {
			candidate := from.Add(windowDuration(s.MaxWait))
			if candidate.After(expiry) {
				expiry = candidate
			}
		}
	}
	return expiry
}

func windowDuration(w *ruleModel.TimeWindow) time.Duration {
	switch strings.ToLower(w.Unit) {
	case "minutes":
		return time.Duration(w.Value) * time.Minute
	case "hours":
		return time.Duration(w.Value) * time.Hour
	case "days":
		return time.Duration(w.Value) * 24 * time.Hour
	default:
		return time.Duration(w.Value) * time.Minute
	}
}

func (p *CEPUsecase) findPattern(id string) (model.CEPPattern, bool) {
	for _, pat := range p.patterns {
		if pat.ID == id {
			return pat, true
		}
	}
	return model.CEPPattern{}, false
}
