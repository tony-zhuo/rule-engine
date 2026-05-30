package core

import (
	"context"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// EventProducer publishes events into an MQ. Symmetric counterpart to
// EventConsumer (consumer.go): the engine's I/O boundary is an interface so the
// MQ choice (NATS / Kafka) lives in pluggable backends, not in the engine core.
//
// Used by:
//   - cmd/event-producer (load generator for benchmarks + dev)
//   - Task L (shadow traffic comparator publishes to both backends at once)
//
// Producers compute the shard themselves (via ShardOfMember in keygroup.go) so
// the routing convention stays in one place — consumer's FilterSubject and
// producer's published subject must agree.
type EventProducer interface {
	Publish(ctx context.Context, event *behaviorModel.BehaviorEvent) error
	Close() error
}
