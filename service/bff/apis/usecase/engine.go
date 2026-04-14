package usecase

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

type EngineUsecaseInterface interface {
	// Rule management
	CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error)
	GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error)
	ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error)
	UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error
	SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error
	// Event processing
	CheckEvent(ctx context.Context, req *CheckEventReq) (*CheckEventResp, error)
}

type CheckEventReq struct {
	EventID    string                     `json:"event_id"`
	MemberID   string                     `json:"member_id"   binding:"required"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"    binding:"required"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
}

type CheckEventResp struct {
	EventID         string               `json:"event_id"`
	MatchedRules    []MatchedRuleResp    `json:"matched_rules"`
	MatchedPatterns []MatchedPatternResp `json:"matched_patterns,omitempty"`
	ProcessedAt     time.Time            `json:"processed_at"`
}

type MatchedRuleResp struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type MatchedPatternResp struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Variables map[string]any `json:"variables,omitempty"`
}

var (
	_engineUCOnce sync.Once
	_engineUCObj  *EngineUsecase
)

var _ EngineUsecaseInterface = (*EngineUsecase)(nil)

type EngineUsecase struct {
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
	eventStore     behaviorModel.BehaviorEventStoreInterface
	cepProcessor   cepModel.ProcessorInterface
}

func NewEngineUsecase(
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	eventStore behaviorModel.BehaviorEventStoreInterface,
	cepProcessor cepModel.ProcessorInterface,
) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{
			ruleStrategyUC: ruleStrategyUC,
			eventStore:     eventStore,
			cepProcessor:   cepProcessor,
		}
	})
	return _engineUCObj
}

func (uc *EngineUsecase) CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Create(ctx, req)
}

func (uc *EngineUsecase) GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Get(ctx, id)
}

func (uc *EngineUsecase) ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.List(ctx, status)
}

func (uc *EngineUsecase) UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error {
	return uc.ruleStrategyUC.Update(ctx, id, req)
}

func (uc *EngineUsecase) SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error {
	return uc.ruleStrategyUC.SetStatus(ctx, id, status)
}

// CheckEvent evaluates rules and CEP patterns synchronously against an event.
// The hot path uses Redis only (no PostgreSQL).
func (uc *EngineUsecase) CheckEvent(ctx context.Context, req *CheckEventReq) (*CheckEventResp, error) {
	// 1. Defaults.
	if req.EventID == "" {
		req.EventID = uuid.New().String()
	}
	if req.OccurredAt.IsZero() {
		req.OccurredAt = time.Now()
	}

	// 2. List active compiled rules (Redis cache, pre-computed maxWindow).
	ruleSet, err := uc.ruleStrategyUC.ListActiveCompiled(ctx)
	if err != nil {
		return nil, fmt.Errorf("check event: list compiled rules: %w", err)
	}

	// 3. Build aggregate conditions using pre-computed keys.
	allKeys := ruleUsecase.CollectUniqueAggregateKeys(ruleSet.Strategies)
	maxWindow := ruleSet.MaxWindow
	if maxWindow == 0 {
		maxWindow = 24 * time.Hour // safety floor
	}
	now := time.Now()
	conds := ruleUsecase.BuildAggregateConds(req.MemberID, allKeys, now)

	// 4. Store event + batch aggregate in a single Redis pipeline round-trip.
	// Schemas drive the zero-alloc pipe-separated encoding; the map can be
	// nil/empty when no behavior has numeric aggregate conditions.
	aggResults, err := uc.eventStore.StoreAndAggregate(ctx, &behaviorModel.BehaviorEvent{
		EventID:    req.EventID,
		MemberID:   req.MemberID,
		Behavior:   req.Behavior,
		Fields:     req.Fields,
		OccurredAt: req.OccurredAt,
	}, ruleSet.Schemas, conds, maxWindow)
	if err != nil {
		return nil, fmt.Errorf("check event: store and aggregate: %w", err)
	}
	cache := make(map[string]any, len(aggResults))
	for k, v := range aggResults {
		cache[k] = v
	}

	// 5. Evaluate compiled rules — in-memory closures.
	fields := make(map[string]any, len(req.Fields)+2)
	for k, v := range req.Fields {
		fields[k] = v
	}
	fields["behavior"] = string(req.Behavior)
	fields["member_id"] = req.MemberID

	var matchedRules []MatchedRuleResp
	for _, cs := range ruleSet.Strategies {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, cache)
		ok, evalErr := cs.Eval(evalCtx)
		if evalErr != nil || !ok {
			continue
		}
		matchedRules = append(matchedRules, MatchedRuleResp{ID: cs.ID, Name: cs.Name})
	}

	// 7. CEP pattern matching (Redis state).
	var matchedPatterns []MatchedPatternResp
	cepEvent := &cepModel.Event{
		EventID:    req.EventID,
		MemberID:   req.MemberID,
		Behavior:   string(req.Behavior),
		Fields:     req.Fields,
		OccurredAt: req.OccurredAt,
	}
	cepResults, err := uc.cepProcessor.ProcessEvent(ctx, cepEvent)
	if err != nil {
		return nil, fmt.Errorf("check event: cep: %w", err)
	}
	for _, r := range cepResults {
		matchedPatterns = append(matchedPatterns, MatchedPatternResp{
			ID:        r.PatternID,
			Name:      r.PatternName,
			Variables: r.Variables,
		})
	}

	// 8. Build response.
	resp := &CheckEventResp{
		EventID:         req.EventID,
		MatchedRules:    matchedRules,
		MatchedPatterns: matchedPatterns,
		ProcessedAt:     time.Now(),
	}

	// NOTE: Kafka write-back to PG is intentionally omitted here. CheckEvent is
	// the hot path optimized for throughput — any per-request work (marshal,
	// Kafka enqueue, goroutine creation) is pure overhead. If PG audit data is
	// needed, it must come from a separate source (e.g. a periodic batch job
	// reading from Redis, or the async POST /v1/events endpoint).
	return resp, nil
}
