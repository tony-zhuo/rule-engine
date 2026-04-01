package usecase

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
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
	ProcessEvent(ctx context.Context, req *ProcessEventReq) (*ProcessEventResp, error)
}

type ProcessEventReq struct {
	MemberID   string                    `json:"member_id"   binding:"required"`
	PlatformID string                    `json:"platform_id"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"    binding:"required"`
	Fields     map[string]any            `json:"fields"`
	OccurredAt time.Time                 `json:"occurred_at"`
}

type ProcessEventResp struct {
	MatchedRules []MatchedRule `json:"matched_rules"`
}

type MatchedRule struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

var (
	_engineUCOnce sync.Once
	_engineUCObj  *EngineUsecase
)

var _ EngineUsecaseInterface = (*EngineUsecase)(nil)

type EngineUsecase struct {
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
	behaviorUC     behaviorModel.BehaviorUsecaseInterface
}

func NewEngineUsecase(
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	behaviorUC behaviorModel.BehaviorUsecaseInterface,
) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{
			ruleStrategyUC: ruleStrategyUC,
			behaviorUC:     behaviorUC,
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

func (uc *EngineUsecase) ProcessEvent(ctx context.Context, req *ProcessEventReq) (*ProcessEventResp, error) {
	occurredAt := req.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	if _, err := uc.behaviorUC.Log(ctx, &behaviorModel.LogBehaviorReq{
		MemberID:   req.MemberID,
		PlatformID: req.PlatformID,
		Behavior:   req.Behavior,
		Fields:     req.Fields,
		OccurredAt: occurredAt,
	}); err != nil {
		return nil, fmt.Errorf("process event: log behavior: %w", err)
	}

	rules, err := uc.ruleStrategyUC.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("process event: list active rules: %w", err)
	}

	// Collect all aggregate keys from all rules and deduplicate.
	var allKeys []ruleModel.AggregateKey
	seen := make(map[string]struct{})
	for _, rule := range rules {
		for _, k := range ruleModel.CollectAggregateKeys(rule.RuleNode) {
			ck := k.CacheKey()
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				allKeys = append(allKeys, k)
			}
		}
	}

	// Batch query all aggregation results.
	cache := make(map[string]any, len(allKeys))
	now := time.Now()
	for _, k := range allKeys {
		cond := buildAggregateCond(req.MemberID, k, now)
		result, err := uc.behaviorUC.Aggregate(ctx, cond)
		if err != nil {
			continue
		}
		cache[k.CacheKey()] = result
	}

	// Evaluate rules with preloaded context.
	resp := &ProcessEventResp{}
	for _, rule := range rules {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(req.Fields, cache)
		matched, err := uc.ruleStrategyUC.Evaluate(rule.RuleNode, evalCtx)
		if err != nil {
			continue
		}
		if matched {
			resp.MatchedRules = append(resp.MatchedRules, MatchedRule{ID: rule.ID, Name: rule.Name})
		}
	}
	return resp, nil
}

func buildAggregateCond(memberID string, k ruleModel.AggregateKey, now time.Time) *behaviorModel.AggregateCond {
	parts := strings.SplitN(k.Field, ":", 3)
	fieldPath := ""
	if len(parts) == 3 {
		fieldPath = parts[2]
	}

	var since time.Time
	if k.Window != nil {
		switch strings.ToLower(k.Window.Unit) {
		case "minutes":
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		case "hours":
			since = now.Add(-time.Duration(k.Window.Value) * time.Hour)
		case "days":
			since = now.Add(-time.Duration(k.Window.Value) * 24 * time.Hour)
		default:
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		}
	}

	return &behaviorModel.AggregateCond{
		MemberID:    memberID,
		Behavior:    behaviorModel.BehaviorType(parts[0]),
		Aggregation: strings.ToUpper(parts[1]),
		FieldPath:   fieldPath,
		Since:       since,
	}
}
