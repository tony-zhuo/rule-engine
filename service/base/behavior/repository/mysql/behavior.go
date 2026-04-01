package mysql

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"

	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

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
	if err := r.db.WithContext(ctx).Create(obj).Error; err != nil {
		return fmt.Errorf("behavior repo create: %w", err)
	}
	return nil
}

func (r *BehaviorRepo) Aggregate(ctx context.Context, cond *model.AggregateCond) (float64, error) {
	q := r.db.WithContext(ctx).Model(&model.BehaviorLog{}).
		Where("member_id = ? AND behavior = ? AND occurred_at >= ?",
			cond.MemberID, cond.Behavior, cond.Since)

	var result float64
	agg := strings.ToUpper(cond.Aggregation)
	if agg == "COUNT" || cond.FieldPath == "" {
		q = q.Select("COALESCE(COUNT(*), 0)")
	} else {
		q = q.Select(fmt.Sprintf("COALESCE(%s(JSON_EXTRACT(fields, '$.%s')), 0)", agg, cond.FieldPath))
	}
	if err := q.Scan(&result).Error; err != nil {
		return 0, fmt.Errorf("aggregate: %w", err)
	}
	return result, nil
}
