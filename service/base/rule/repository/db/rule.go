package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_ruleRepoOnce sync.Once
	_ruleRepoObj  *RuleStrategyRepo
)

var _ model.RuleStrategyRepoInterface = (*RuleStrategyRepo)(nil)

type RuleStrategyRepo struct {
	db *gorm.DB
}

func NewRuleStrategyRepo(db *gorm.DB) *RuleStrategyRepo {
	_ruleRepoOnce.Do(func() {
		_ruleRepoObj = &RuleStrategyRepo{db: db}
	})
	return _ruleRepoObj
}

// NewRuleStrategyRepoWith creates a non-singleton instance (for testing with alternative DB connections).
func NewRuleStrategyRepoWith(db *gorm.DB) *RuleStrategyRepo {
	return &RuleStrategyRepo{db: db}
}

func marshalRuleNode(obj *model.RuleStrategy) error {
	b, err := json.Marshal(obj.RuleNode)
	if err != nil {
		return fmt.Errorf("marshal rule_node: %w", err)
	}
	obj.RuleNodeJSON = string(b)
	return nil
}

func unmarshalRuleNode(obj *model.RuleStrategy) error {
	if err := json.Unmarshal([]byte(obj.RuleNodeJSON), &obj.RuleNode); err != nil {
		return fmt.Errorf("unmarshal rule_node: %w", err)
	}
	return nil
}

func (r *RuleStrategyRepo) Get(ctx context.Context, id uint64) (*model.RuleStrategy, error) {
	var obj model.RuleStrategy
	if err := r.db.WithContext(ctx).First(&obj, id).Error; err != nil {
		return nil, fmt.Errorf("rule repo get: %w", err)
	}
	if err := unmarshalRuleNode(&obj); err != nil {
		return nil, err
	}
	return &obj, nil
}

func (r *RuleStrategyRepo) List(ctx context.Context, status *model.RuleStrategyStatus) ([]*model.RuleStrategy, error) {
	var objs []*model.RuleStrategy
	q := r.db.WithContext(ctx)
	if status != nil {
		q = q.Where("status = ?", *status)
	}
	if err := q.Find(&objs).Error; err != nil {
		return nil, fmt.Errorf("rule repo list: %w", err)
	}
	for _, obj := range objs {
		if err := unmarshalRuleNode(obj); err != nil {
			return nil, err
		}
	}
	return objs, nil
}

func (r *RuleStrategyRepo) Create(ctx context.Context, obj *model.RuleStrategy) error {
	if err := marshalRuleNode(obj); err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(obj).Error; err != nil {
		return fmt.Errorf("rule repo create: %w", err)
	}
	return nil
}

func (r *RuleStrategyRepo) Update(ctx context.Context, id uint64, updates map[string]any) error {
	if err := r.db.WithContext(ctx).Model(&model.RuleStrategy{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return fmt.Errorf("rule repo update: %w", err)
	}
	return nil
}
