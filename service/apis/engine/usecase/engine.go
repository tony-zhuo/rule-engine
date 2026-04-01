package usecase

import (
	"context"
	"fmt"
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

	resp := &ProcessEventResp{}
	for _, rule := range rules {
		evalCtx := ruleUsecase.NewDBEvalContext(req.MemberID, req.Fields, uc.behaviorUC)
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
