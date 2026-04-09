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

// ProcessedEventRepoInterface tracks fully-processed events for dedup on retry.
type ProcessedEventRepoInterface interface {
	// Upsert inserts a new pending record or increments attempts if it already exists.
	// Returns the current ProcessedEvent state.
	Upsert(ctx context.Context, eventID string) (*ProcessedEvent, error)
	// MarkCompleted sets status to completed.
	MarkCompleted(ctx context.Context, eventID string) error
	// MarkFailed sets status to failed.
	MarkFailed(ctx context.Context, eventID string) error
}
