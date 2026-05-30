package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_ruleStrategyUCOnce sync.Once
	_ruleStrategyUCObj  *RuleStrategyUsecase
)

var _ model.RuleStrategyUsecaseInterface = (*RuleStrategyUsecase)(nil)

// RuleStrategyUsecase is the rule-store usecase used by both cmd/apis (rule CRUD)
// and cmd/rule-engine-core (loads rules at startup). The Redis cache that the
// old multi-instance API setup needed was removed in Task Q — both consumers
// are now single-process, so the in-process atomic.Pointer (`cached`) is
// sufficient. Dropping the rdb field also unlinks go-redis from both binaries.
type RuleStrategyUsecase struct {
	repo   model.RuleStrategyRepoInterface
	ruleUC *RuleUsecase
	cached atomic.Pointer[model.CompiledRuleSet] // lock-free read on hot path
}

func NewRuleStrategyUsecase(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase) *RuleStrategyUsecase {
	_ruleStrategyUCOnce.Do(func() {
		_ruleStrategyUCObj = &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC}
	})
	return _ruleStrategyUCObj
}

// NewRuleStrategyUsecaseWith creates a non-singleton instance (for tests).
func NewRuleStrategyUsecaseWith(repo model.RuleStrategyRepoInterface, ruleUC *RuleUsecase) *RuleStrategyUsecase {
	return &RuleStrategyUsecase{repo: repo, ruleUC: ruleUC}
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
	uc.invalidateCache()
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
	uc.invalidateCache()
	return nil
}

func (uc *RuleStrategyUsecase) SetStatus(ctx context.Context, id uint64, status model.RuleStrategyStatus) error {
	if err := uc.repo.Update(ctx, id, map[string]any{"status": status}); err != nil {
		return err
	}
	uc.invalidateCache()
	return nil
}

// ListActive returns active rules directly from PG. The previous Redis cache
// layer (for cross-instance sharing) was removed in Task Q — both consumers of
// this usecase are single-process; the atomic.Pointer cache in ListActiveCompiled
// is sufficient.
func (uc *RuleStrategyUsecase) ListActive(ctx context.Context) ([]*model.RuleStrategy, error) {
	s := model.RuleStrategyStatusActive
	return uc.repo.List(ctx, &s)
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

func (uc *RuleStrategyUsecase) invalidateCache() {
	uc.cached.Store(nil) // next ListActiveCompiled triggers a recompile
}

func (uc *RuleStrategyUsecase) Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	return uc.ruleUC.Evaluate(node, ctx)
}
