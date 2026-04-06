package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	rulemodel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var ErrDuplicateEvent = errors.New("duplicate event")

var (
	_behaviorRepoOnce sync.Once
	_behaviorRepoObj  *BehaviorRepo
)

var _ model.BehaviorRepoInterface = (*BehaviorRepo)(nil)

type BehaviorRepo struct {
	db *gorm.DB
}

func NewBehaviorRepo(db *gorm.DB) *BehaviorRepo {
	_behaviorRepoOnce.Do(func() {
		_behaviorRepoObj = &BehaviorRepo{db: db}
	})
	return _behaviorRepoObj
}

func (r *BehaviorRepo) Create(ctx context.Context, obj *model.BehaviorLog) error {
	result := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).
		Create(obj)
	if result.Error != nil {
		return fmt.Errorf("behavior repo create: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

func (r *BehaviorRepo) Aggregate(ctx context.Context, cond *model.AggregateCond) (float64, error) {
	q := r.db.WithContext(ctx).Model(&model.BehaviorLog{}).
		Where("member_id = ? AND behavior = ? AND occurred_at >= ?",
			cond.MemberID, cond.Behavior, cond.Since)

	var result float64
	agg := strings.ToUpper(cond.Aggregation)

	// Defense-in-depth: validate aggregation and field path before interpolating into SQL.
	if err := rulemodel.ValidateAggregation(agg); err != nil {
		return 0, fmt.Errorf("aggregate: %w", err)
	}
	if err := rulemodel.ValidateFieldPath(cond.FieldPath); err != nil {
		return 0, fmt.Errorf("aggregate: %w", err)
	}

	if agg == "COUNT" || cond.FieldPath == "" {
		q = q.Select("COALESCE(COUNT(*), 0)")
	} else {
		// PostgreSQL JSONB syntax: (fields->>'field_path')::numeric
		q = q.Select(fmt.Sprintf("COALESCE(%s((fields->>'%s')::numeric), 0)", agg, cond.FieldPath))
	}
	if err := q.Scan(&result).Error; err != nil {
		return 0, fmt.Errorf("aggregate: %w", err)
	}
	return result, nil
}
