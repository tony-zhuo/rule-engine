package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_ruleStrategyUCOnce sync.Once
	_ruleStrategyUCObj  *RuleStrategyUsecase
)

var _ model.RuleStrategyUsecaseInterface = (*RuleStrategyUsecase)(nil)

type RuleStrategyUsecase struct {
	repo   model.RuleStrategyRepoInterface
	ruleUC *RuleUsecase
}

func NewRuleStrategyUsecase(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase) *RuleStrategyUsecase {
	_ruleStrategyUCOnce.Do(func() {
		_ruleStrategyUCObj = &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC}
	})
	return _ruleStrategyUCObj
}

func (uc *RuleStrategyUsecase) Get(ctx context.Context, id uint64) (*model.RuleStrategy, error) {
	return uc.repo.Get(ctx, id)
}

func (uc *RuleStrategyUsecase) List(ctx context.Context, status *model.RuleStrategyStatus) ([]*model.RuleStrategy, error) {
	return uc.repo.List(ctx, status)
}

func (uc *RuleStrategyUsecase) Create(ctx context.Context, req *model.CreateRuleStrategyReq) (*model.RuleStrategy, error) {
	obj := &model.RuleStrategy{
		Name:        req.Name,
		Description: req.Description,
		RuleNode:    req.RuleNode,
		Status:      model.RuleStrategyStatusActive,
	}
	if err := uc.repo.Create(ctx, obj); err != nil {
		return nil, fmt.Errorf("strategy usecase create: %w", err)
	}
	return obj, nil
}

func (uc *RuleStrategyUsecase) Update(ctx context.Context, id uint64, req *model.UpdateRuleStrategyReq) error {
	updates := make(map[string]any)
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.RuleNode != nil {
		b, err := json.Marshal(req.RuleNode)
		if err != nil {
			return fmt.Errorf("strategy usecase update marshal rule_node: %w", err)
		}
		updates["rule_node"] = string(b)
	}
	if len(updates) == 0 {
		return nil
	}
	return uc.repo.Update(ctx, id, updates)
}

func (uc *RuleStrategyUsecase) SetStatus(ctx context.Context, id uint64, status model.RuleStrategyStatus) error {
	return uc.repo.Update(ctx, id, map[string]any{"status": status})
}

func (uc *RuleStrategyUsecase) ListActive(ctx context.Context) ([]*model.RuleStrategy, error) {
	s := model.RuleStrategyStatusActive
	return uc.repo.List(ctx, &s)
}

func (uc *RuleStrategyUsecase) Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	return uc.ruleUC.Evaluate(node, ctx)
}
