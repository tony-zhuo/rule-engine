package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_ruleStrategyUCOnce sync.Once
	_ruleStrategyUCObj  *RuleStrategyUsecase
)

const activeRulesCacheKey = "rule_engine:active_rules"
const activeRulesCacheTTL = 60 * time.Second

var _ model.RuleStrategyUsecaseInterface = (*RuleStrategyUsecase)(nil)

type RuleStrategyUsecase struct {
	repo    model.RuleStrategyRepoInterface
	ruleUC  *RuleUsecase
	rdb     *redis.Client
}

func NewRuleStrategyUsecase(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase, rdb *redis.Client) *RuleStrategyUsecase {
	_ruleStrategyUCOnce.Do(func() {
		_ruleStrategyUCObj = &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC, rdb: rdb}
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
	if err := model.ValidateRuleNode(req.RuleNode); err != nil {
		return nil, fmt.Errorf("strategy usecase create: %w", err)
	}
	obj := &model.RuleStrategy{
		Name:        req.Name,
		Description: req.Description,
		RuleNode:    req.RuleNode,
		Status:      model.RuleStrategyStatusActive,
	}
	if err := uc.repo.Create(ctx, obj); err != nil {
		return nil, fmt.Errorf("strategy usecase create: %w", err)
	}
	uc.invalidateCache(ctx)
	return obj, nil
}

func (uc *RuleStrategyUsecase) Update(ctx context.Context, id uint64, req *model.UpdateRuleStrategyReq) error {
	if req.RuleNode != nil {
		if err := model.ValidateRuleNode(*req.RuleNode); err != nil {
			return fmt.Errorf("strategy usecase update: %w", err)
		}
	}
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
	if err := uc.repo.Update(ctx, id, updates); err != nil {
		return err
	}
	uc.invalidateCache(ctx)
	return nil
}

func (uc *RuleStrategyUsecase) SetStatus(ctx context.Context, id uint64, status model.RuleStrategyStatus) error {
	if err := uc.repo.Update(ctx, id, map[string]any{"status": status}); err != nil {
		return err
	}
	uc.invalidateCache(ctx)
	return nil
}

func (uc *RuleStrategyUsecase) ListActive(ctx context.Context) ([]*model.RuleStrategy, error) {
	// Try Redis cache first.
	cached, err := uc.rdb.Get(ctx, activeRulesCacheKey).Bytes()
	if err == nil {
		var rules []*model.RuleStrategy
		if json.Unmarshal(cached, &rules) == nil {
			return rules, nil
		}
	}

	// Cache miss — query DB.
	s := model.RuleStrategyStatusActive
	rules, err := uc.repo.List(ctx, &s)
	if err != nil {
		return nil, err
	}

	// Write back to Redis (best-effort).
	if data, err := json.Marshal(rules); err == nil {
		uc.rdb.Set(ctx, activeRulesCacheKey, data, activeRulesCacheTTL)
	}
	return rules, nil
}

func (uc *RuleStrategyUsecase) invalidateCache(ctx context.Context) {
	uc.rdb.Del(ctx, activeRulesCacheKey)
}

func (uc *RuleStrategyUsecase) Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	return uc.ruleUC.Evaluate(node, ctx)
}
