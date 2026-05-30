package core

import (
	"fmt"
	"slices"
	"strings"
	"time"

	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

// compiledPattern is a CEP pattern with each state's condition pre-compiled to a
// closure once at registration, so matching an event is just a function call.
type compiledPattern struct {
	pattern        cepModel.CEPPattern
	compiledStates []ruleModel.CompiledRule // one per state, index-aligned
}

// maxDuration is the longest a progress of this pattern may live. MaxWait on each
// state is measured from the progress start, so the largest one bounds the whole
// pattern — used by the watermark-driven sweep to drop stuck progresses.
func (cp *compiledPattern) maxDuration() time.Duration {
	var max time.Duration
	for _, st := range cp.pattern.States {
		if st.MaxWait != nil {
			if d := st.MaxWait.Duration(); d > max {
				max = d
			}
		}
	}
	return max
}

// AddPattern registers a CEP pattern, compiling each state's condition up front.
func (c *Core) AddPattern(pattern cepModel.CEPPattern) error {
	compiled := make([]ruleModel.CompiledRule, len(pattern.States))
	for i, st := range pattern.States {
		fn, err := ruleUsecase.Compile(st.Condition)
		if err != nil {
			return fmt.Errorf("cep: compile pattern %s state %q: %w", pattern.ID, st.Name, err)
		}
		compiled[i] = fn
	}
	c.patterns = append(c.patterns, compiledPattern{pattern: pattern, compiledStates: compiled})
	return nil
}

func (c *Core) patternByID(id string) *compiledPattern {
	for i := range c.patterns {
		if c.patterns[i].pattern.ID == id {
			return &c.patterns[i]
		}
	}
	return nil
}

// deterministicProgressID derives a progress's ID from its origin, replacing a
// random UUID. This makes progress creation reproducible on replay (same inputs
// → same IDs → same state) and doubles as the start-dedup key: the same start
// event can never open two instances of the same pattern for the same member.
func deterministicProgressID(patternID, memberID, startEventID string) string {
	return patternID + "|" + memberID + "|" + startEventID
}

// processCEP advances all of the member's in-flight progresses against the event
// and tries to start new ones. Returns any patterns that completed. All state is
// the member's in-memory Progresses map — no Redis round-trips.
func (c *Core) processCEP(ms *MemberState, event *cepModel.Event, watermark time.Time) []*cepModel.MatchResult {
	// Drop this member's progresses that can no longer complete (pure-timeout
	// case: no event ever arrived to advance or expire them). Correctness does
	// not depend on this — advanceProgress also checks expiry — it bounds memory.
	c.sweepExpiredProgresses(ms, watermark)

	var results []*cepModel.MatchResult

	// Action A: advance existing in-progress instances.
	for id, progress := range ms.Progresses {
		result, remove := c.advanceProgress(ms, event, progress)
		if remove {
			delete(ms.Progresses, id)
		}
		if result != nil {
			results = append(results, result)
		}
	}

	// Action B: try to start new instances from state[0] of each pattern.
	for i := range c.patterns {
		cp := &c.patterns[i]
		if len(cp.pattern.States) == 0 {
			continue
		}
		matched, err := cepMatchState(event, cp.compiledStates[0], nil)
		if err != nil || !matched {
			continue
		}

		id := deterministicProgressID(cp.pattern.ID, event.MemberID, event.EventID)
		if _, exists := ms.Progresses[id]; exists {
			continue // start-dedup: this exact start was already seen (replay safety)
		}

		vars := extractBindings(event, cp.pattern.States[0].ContextBinding)

		// Single-state pattern is an instant match.
		if len(cp.pattern.States) == 1 {
			results = append(results, &cepModel.MatchResult{
				PatternID:   cp.pattern.ID,
				PatternName: cp.pattern.Name,
				MemberID:    event.MemberID,
				Variables:   vars,
				MatchedAt:   event.OccurredAt,
			})
			continue
		}

		ms.Progresses[id] = &cepModel.PatternProgress{
			ID:              id,
			PatternID:       cp.pattern.ID,
			MemberID:        event.MemberID,
			CurrentStep:     1, // state[0] already matched
			Variables:       vars,
			StartedAt:       event.OccurredAt, // event time, not wall clock — replay-correct
			ProcessedEvents: []string{event.EventID},
		}
	}

	return results
}

// advanceProgress checks whether the event satisfies a progress's next expected
// state. Returns the completed match (if any) and whether the progress should be
// removed (completed or expired).
func (c *Core) advanceProgress(ms *MemberState, event *cepModel.Event, progress *cepModel.PatternProgress) (*cepModel.MatchResult, bool) {
	cp := c.patternByID(progress.PatternID)
	if cp == nil {
		return nil, true // pattern no longer exists → drop the progress
	}
	if progress.CurrentStep >= len(cp.pattern.States) {
		return nil, true
	}

	// Idempotency: this event already advanced this progress (replay/redelivery).
	if slices.Contains(progress.ProcessedEvents, event.EventID) {
		return nil, false
	}

	state := cp.pattern.States[progress.CurrentStep]

	// Expiry by event time: MaxWait is measured from the progress start.
	if state.MaxWait != nil {
		deadline := progress.StartedAt.Add(state.MaxWait.Duration())
		if event.OccurredAt.After(deadline) {
			return nil, true // window blown → drop
		}
	}

	matched, err := cepMatchState(event, cp.compiledStates[progress.CurrentStep], progress.Variables)
	if err != nil || !matched {
		return nil, false // event doesn't match the next step → leave progress as is
	}

	// Merge any newly bound variables, then advance.
	for k, v := range extractBindings(event, state.ContextBinding) {
		progress.Variables[k] = v
	}
	progress.CurrentStep++
	progress.ProcessedEvents = append(progress.ProcessedEvents, event.EventID)

	if progress.CurrentStep >= len(cp.pattern.States) {
		return &cepModel.MatchResult{
			PatternID:   cp.pattern.ID,
			PatternName: cp.pattern.Name,
			MemberID:    event.MemberID,
			Variables:   progress.Variables,
			MatchedAt:   event.OccurredAt,
		}, true // fully matched → emit + remove
	}
	return nil, false // advanced, keep waiting
}

// sweepExpiredProgresses drops progresses whose pattern can no longer complete
// before the watermark — the timeout case an event would never clear on its own.
func (c *Core) sweepExpiredProgresses(ms *MemberState, watermark time.Time) {
	for id, p := range ms.Progresses {
		cp := c.patternByID(p.PatternID)
		if cp == nil {
			delete(ms.Progresses, id)
			continue
		}
		if d := cp.maxDuration(); d > 0 && watermark.Sub(p.StartedAt) > d {
			delete(ms.Progresses, id)
		}
	}
}

// cepMatchState evaluates a compiled state condition against the event, exposing
// "behavior"/"member_id" as fields and injecting accumulated bindings as variables.
func cepMatchState(event *cepModel.Event, compiled ruleModel.CompiledRule, vars map[string]any) (bool, error) {
	fields := make(map[string]any, len(event.Fields)+2)
	for k, v := range event.Fields {
		fields[k] = v
	}
	fields["behavior"] = event.Behavior
	fields["member_id"] = event.MemberID

	evalCtx := ruleModel.NewMapContext(fields)
	if len(vars) > 0 {
		evalCtx = evalCtx.WithVariables(vars)
	}
	return compiled(evalCtx)
}

// extractBindings resolves "$event.field" context bindings from the event.
func extractBindings(event *cepModel.Event, bindings map[string]string) map[string]any {
	result := make(map[string]any, len(bindings))
	for varName, src := range bindings {
		fieldPath := strings.TrimPrefix(src, "$event.")
		if v, ok := event.Fields[fieldPath]; ok {
			result[varName] = v
		}
	}
	return result
}
