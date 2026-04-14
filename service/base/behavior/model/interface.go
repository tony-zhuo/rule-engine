package model

import (
	"context"
	"time"
)

type BehaviorRepoInterface interface {
	Create(ctx context.Context, obj *BehaviorLog) error
	// Aggregate executes a COUNT/SUM/AVG/MAX/MIN query on behavior_logs.
	// field is the JSON field path to aggregate (empty string for COUNT).
	// Returns float64 result.
	Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
	// BatchAggregate merges multiple aggregation conditions into a single SQL
	// using PostgreSQL FILTER clauses. Returns a map keyed by AggregateCond.CacheKey().
	BatchAggregate(ctx context.Context, memberID string, conds []AggregateCond) (map[string]float64, error)
}

type BehaviorUsecaseInterface interface {
	Log(ctx context.Context, req *LogBehaviorReq) (*BehaviorLog, error)
	Aggregate(ctx context.Context, cond *AggregateCond) (float64, error)
	BatchAggregate(ctx context.Context, memberID string, conds []AggregateCond) (map[string]float64, error)
}

// BehaviorEventStoreInterface is the Redis-backed real-time event store
// that replaces PostgreSQL on the hot path for event storage and aggregation.
type BehaviorEventStoreInterface interface {
	// StoreEvent adds an event to the member's behavior sorted set in Redis.
	// maxWindow controls how long events are retained (pruning + TTL).
	// Returns true if the event was newly inserted (not a duplicate).
	StoreEvent(ctx context.Context, event *BehaviorEvent, maxWindow time.Duration) (bool, error)
	// BatchAggregate computes multiple aggregations from Redis sorted sets.
	// Returns a map keyed by AggregateCond.Key with float64 results.
	BatchAggregate(ctx context.Context, memberID string, conds []AggregateCond) (map[string]float64, error)
	// StoreAndAggregate combines StoreEvent + BatchAggregate into a single Redis
	// pipeline round-trip. maxWindow controls event retention.
	StoreAndAggregate(ctx context.Context, event *BehaviorEvent, conds []AggregateCond, maxWindow time.Duration) (map[string]float64, error)
}

// ProcessedEventRepoInterface tracks fully-processed events for dedup on retry.
type ProcessedEventRepoInterface interface {
	// Upsert inserts a new pending record or increments attempts if it already exists.
	// Returns the current ProcessedEvent state.
	Upsert(ctx context.Context, eventID string) (*ProcessedEvent, error)
	// UpsertWithBehaviorLog combines UPSERT processed_events + INSERT behavior_log
	// into a single DB round-trip using a CTE. The behavior_log INSERT only executes
	// when the event status is pending.
	UpsertWithBehaviorLog(ctx context.Context, eventID string, log *BehaviorLog) (*ProcessedEvent, error)
	// MarkCompleted sets status to completed.
	MarkCompleted(ctx context.Context, eventID string) error
	// MarkFailed sets status to failed.
	MarkFailed(ctx context.Context, eventID string) error
}
