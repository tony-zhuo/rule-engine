// Package usecase hosts the cmd/apis usecases. After Task M removed CheckEvent,
// the BFF API surface is rule CRUD only — the real event-processing engine is
// cmd/rule-engine-core (NATS consumer + in-memory state).
package usecase

import (
	"context"
	"sync"

	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// EngineUsecaseInterface is, after Task M, a rule-admin surface — kept under
// the historical name to avoid churning the wire bindings + controller imports
// in this commit. A follow-up rename to RuleAdminUsecase is tracked separately.
type EngineUsecaseInterface interface {
	CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error)
	GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error)
	ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error)
	UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error
	SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error
}

var (
	_engineUCOnce sync.Once
	_engineUCObj  *EngineUsecase
)

var _ EngineUsecaseInterface = (*EngineUsecase)(nil)

// EngineUsecase delegates rule CRUD to the strategy usecase. CheckEvent was
// removed in Task M — event processing now happens out-of-process in
// cmd/rule-engine-core, driven by a NATS / Kafka consumer.
type EngineUsecase struct {
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
}

func NewEngineUsecase(ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{ruleStrategyUC: ruleStrategyUC}
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
