package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

var (
	_behaviorUCOnce sync.Once
	_behaviorUCObj  *BehaviorUsecase
)

var _ model.BehaviorUsecaseInterface = (*BehaviorUsecase)(nil)

type BehaviorUsecase struct {
	repo model.BehaviorRepoInterface
}

func NewBehaviorUsecase(repo model.BehaviorRepoInterface) *BehaviorUsecase {
	_behaviorUCOnce.Do(func() {
		_behaviorUCObj = &BehaviorUsecase{repo: repo}
	})
	return _behaviorUCObj
}

func (uc *BehaviorUsecase) Log(ctx context.Context, req *model.LogBehaviorReq) (*model.BehaviorLog, error) {
	fieldsJSON, err := json.Marshal(req.Fields)
	if err != nil {
		return nil, fmt.Errorf("behavior usecase log marshal fields: %w", err)
	}

	occurredAt := req.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	obj := &model.BehaviorLog{
		EventID:    req.EventID,
		MemberID:   req.MemberID,
		Behavior:   req.Behavior,
		Fields:     string(fieldsJSON),
		OccurredAt: occurredAt,
	}
	if err := uc.repo.Create(ctx, obj); err != nil {
		return nil, fmt.Errorf("behavior usecase log: %w", err)
	}
	return obj, nil
}

func (uc *BehaviorUsecase) Aggregate(ctx context.Context, cond *model.AggregateCond) (float64, error) {
	return uc.repo.Aggregate(ctx, cond)
}

func (uc *BehaviorUsecase) BatchAggregate(ctx context.Context, memberID string, conds []model.AggregateCond) (map[string]float64, error) {
	return uc.repo.BatchAggregate(ctx, memberID, conds)
}
