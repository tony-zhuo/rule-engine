// Package usecase hosts the cmd/apis usecases. cmd/apis is the engine's control
// plane: it manages the rule strategies and CEP patterns that cmd/rule-engine-core
// loads from PostgreSQL at startup. Event processing itself lives entirely in
// cmd/rule-engine-core (MQ consumer + in-memory state).
package usecase

import (
	"context"
	"sync"

	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// RuleAdminUsecaseInterface is the rule administration surface exposed over HTTP.
type RuleAdminUsecaseInterface interface {
	CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error)
	GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error)
	ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error)
	UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error
	SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error
}

var (
	_ruleAdminUCOnce sync.Once
	_ruleAdminUCObj  *RuleAdminUsecase
)

var _ RuleAdminUsecaseInterface = (*RuleAdminUsecase)(nil)

// RuleAdminUsecase delegates rule CRUD to the strategy usecase.
type RuleAdminUsecase struct {
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
}

func NewRuleAdminUsecase(ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface) *RuleAdminUsecase {
	_ruleAdminUCOnce.Do(func() {
		_ruleAdminUCObj = &RuleAdminUsecase{ruleStrategyUC: ruleStrategyUC}
	})
	return _ruleAdminUCObj
}

func (uc *RuleAdminUsecase) CreateRule(ctx context.Context, req *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Create(ctx, req)
}

func (uc *RuleAdminUsecase) GetRule(ctx context.Context, id uint64) (*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.Get(ctx, id)
}

func (uc *RuleAdminUsecase) ListRules(ctx context.Context, status *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error) {
	return uc.ruleStrategyUC.List(ctx, status)
}

func (uc *RuleAdminUsecase) UpdateRule(ctx context.Context, id uint64, req *ruleModel.UpdateRuleStrategyReq) error {
	return uc.ruleStrategyUC.Update(ctx, id, req)
}

func (uc *RuleAdminUsecase) SetRuleStatus(ctx context.Context, id uint64, status ruleModel.RuleStrategyStatus) error {
	return uc.ruleStrategyUC.SetStatus(ctx, id, status)
}
