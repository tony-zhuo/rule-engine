package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
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
	repo   model.RuleStrategyRepoInterface
	ruleUC *RuleUsecase
	rdb    *redis.Client
	cached atomic.Pointer[model.CompiledRuleSet] // lock-free read on hot path
}

func NewRuleStrategyUsecase(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase, rdb *redis.Client) *RuleStrategyUsecase {
	_ruleStrategyUCOnce.Do(func() {
		_ruleStrategyUCObj = &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC, rdb: rdb}
	})
	return _ruleStrategyUCObj
}

// NewRuleStrategyUsecaseWith creates a non-singleton instance (for testing with alternative connections).
func NewRuleStrategyUsecaseWith(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase, rdb *redis.Client) *RuleStrategyUsecase {
	return &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC, rdb: rdb}
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

// ListActiveCompiled returns the in-memory compiled rule set.
// On the hot path this is a single atomic pointer load (~1ns, zero alloc).
// The cache is lazily populated on first call and invalidated on rule changes.
func (uc *RuleStrategyUsecase) ListActiveCompiled(ctx context.Context) (*model.CompiledRuleSet, error) {
	if rs := uc.cached.Load(); rs != nil {
		return rs, nil
	}
	return uc.compileAndCache(ctx)
}

func (uc *RuleStrategyUsecase) compileAndCache(ctx context.Context) (*model.CompiledRuleSet, error) {
	rules, err := uc.ListActive(ctx)
	if err != nil {
		return nil, err
	}

	strategies := make([]model.CompiledStrategy, 0, len(rules))
	for _, rule := range rules {
		eval, err := Compile(rule.RuleNode)
		if err != nil {
			return nil, fmt.Errorf("compile rule %d (%s): %w", rule.ID, rule.Name, err)
		}
		strategies = append(strategies, model.CompiledStrategy{
			ID:            rule.ID,
			Name:          rule.Name,
			Eval:          eval,
			AggregateKeys: model.CollectAggregateKeys(rule.RuleNode),
		})
	}

	allKeys := CollectUniqueAggregateKeys(strategies)
	maxWindow := MaxWindowFromKeys(allKeys)
	schemas := BuildBehaviorSchemas(strategies)

	rs := &model.CompiledRuleSet{
		Strategies: strategies,
		MaxWindow:  maxWindow,
		Schemas:    schemas,
	}
	uc.cached.Store(rs)
	return rs, nil
}

func (uc *RuleStrategyUsecase) invalidateCache(ctx context.Context) {
	uc.cached.Store(nil) // clear in-memory cache → next call triggers recompile
	uc.rdb.Del(ctx, activeRulesCacheKey)
}

func (uc *RuleStrategyUsecase) Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	return uc.ruleUC.Evaluate(node, ctx)
}
