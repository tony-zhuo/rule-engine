package db

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	rulemodel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
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
	result := r.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).
		Create(obj)
	if result.Error != nil {
		return fmt.Errorf("behavior repo create: %w", result.Error)
	}
	return nil
}

func (r *BehaviorRepo) BatchAggregate(ctx context.Context, memberID string, conds []model.AggregateCond) (map[string]float64, error) {
	if len(conds) == 0 {
		return map[string]float64{}, nil
	}

	var selectCols []string
	var args []any
	paramIdx := 1

	for _, cond := range conds {
		agg := strings.ToUpper(cond.Aggregation)
		if err := rulemodel.ValidateAggregation(agg); err != nil {
			return nil, fmt.Errorf("batch aggregate: %w", err)
		}
		if err := rulemodel.ValidateFieldPath(cond.FieldPath); err != nil {
			return nil, fmt.Errorf("batch aggregate: %w", err)
		}

		var expr string
		if agg == "COUNT" || cond.FieldPath == "" {
			expr = fmt.Sprintf(
				"COALESCE(COUNT(*) FILTER (WHERE behavior = $%d AND occurred_at >= $%d), 0)",
				paramIdx, paramIdx+1,
			)
		} else {
			expr = fmt.Sprintf(
				"COALESCE(%s((fields->>'%s')::numeric) FILTER (WHERE behavior = $%d AND occurred_at >= $%d), 0)",
				agg, cond.FieldPath, paramIdx, paramIdx+1,
			)
		}
		selectCols = append(selectCols, expr)
		args = append(args, string(cond.Behavior), cond.Since)
		paramIdx += 2
	}

	query := fmt.Sprintf(
		"SELECT %s FROM behavior_logs WHERE member_id = $%d",
		strings.Join(selectCols, ", "), paramIdx,
	)
	args = append(args, memberID)

	rows, err := r.db.WithContext(ctx).Raw(query, args...).Rows()
	if err != nil {
		return nil, fmt.Errorf("batch aggregate query: %w", err)
	}
	defer rows.Close()

	results := make([]float64, len(conds))
	if rows.Next() {
		scanArgs := make([]any, len(conds))
		for i := range results {
			scanArgs[i] = &results[i]
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("batch aggregate scan: %w", err)
		}
	}

	out := make(map[string]float64, len(conds))
	for i, cond := range conds {
		out[cond.Key] = results[i]
	}
	return out, nil
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
