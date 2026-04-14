package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/google/uuid"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
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
	ProcessEvent(ctx context.Context, req *ProcessEventReq) error
	CheckEvent(ctx context.Context, req *CheckEventReq) (*CheckEventResp, error)
}

type ProcessEventReq struct {
	EventID    string                     `json:"event_id"`
	MemberID   string                     `json:"member_id"   binding:"required"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"    binding:"required"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
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
	producer       *kafka.Producer
	eventsTopic    string
	resultsTopic   string
}

func NewEngineUsecase(
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	eventStore behaviorModel.BehaviorEventStoreInterface,
	cepProcessor cepModel.ProcessorInterface,
	producer *kafka.Producer,
	eventsTopic string,
	resultsTopic string,
) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{
			ruleStrategyUC: ruleStrategyUC,
			eventStore:     eventStore,
			cepProcessor:   cepProcessor,
			producer:       producer,
			eventsTopic:    eventsTopic,
			resultsTopic:   resultsTopic,
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

// ProcessEvent produces the event to Kafka for async processing by the worker.
func (uc *EngineUsecase) ProcessEvent(ctx context.Context, req *ProcessEventReq) error {
	if req.EventID == "" {
		req.EventID = uuid.New().String()
	}
	if req.OccurredAt.IsZero() {
		req.OccurredAt = time.Now()
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("process event: marshal: %w", err)
	}

	if err := pkgkafka.Produce(uc.producer, uc.eventsTopic, req.MemberID, data); err != nil {
		return fmt.Errorf("process event: produce: %w", err)
	}
	return nil
}

// CheckEvent evaluates rules and CEP patterns synchronously against an event.
// The hot path uses Redis only (no PostgreSQL). Event data is produced to Kafka
// asynchronously for PG write-back by the worker.
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

	// 9. Fire-and-forget: produce event to Kafka for PG write-back.
	go uc.produceEventAsync(req)

	return resp, nil
}

func (uc *EngineUsecase) produceEventAsync(req *CheckEventReq) {
	data, err := json.Marshal(req)
	if err != nil {
		slog.Error("check event: marshal for kafka", "event_id", req.EventID, "error", err)
		return
	}
	if err := pkgkafka.Produce(uc.producer, uc.eventsTopic, req.MemberID, data); err != nil {
		slog.Error("check event: produce to kafka", "event_id", req.EventID, "error", err)
	}
}
