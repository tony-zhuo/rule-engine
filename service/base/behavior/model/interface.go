package model

import (
	"context"
)

type BehaviorRepoInterface interface {
	Create(ctx context.Context, obj *BehaviorLog) error
	// Aggregate executes a COUNT/SUM/AVG/MAX/MIN query on behavior_logs.
	// field is the JSON field path to aggregate (empty string for COUNT).
	// Returns float64 result.
	Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
}

type BehaviorUsecaseInterface interface {
	Log(ctx context.Context, req *LogBehaviorReq) (*BehaviorLog, error)
	Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
}
