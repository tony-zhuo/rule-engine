package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// NATSProducerConfig configures the NATS JetStream producer.
type NATSProducerConfig struct {
	SubjectPrefix string // subject namespace (e.g. "rule.events")
	NumShards     int    // shard count, for routing: prefix.{shard}.{member}
}

// NATSProducer publishes BehaviorEvents to JetStream. Subject naming follows
// the same convention the NATS consumer (consumer_nats.go) filters on, so
// producer and consumer agree on routing without sharing state.
type NATSProducer struct {
	js  jetstream.JetStream
	cfg NATSProducerConfig
}

var _ EventProducer = (*NATSProducer)(nil)

// NewNATSProducer wires a JetStream context with this shard layout. The caller
// is responsible for ensuring the stream exists (see cmd/event-producer/main.go
// for the typical CreateOrUpdateStream call).
func NewNATSProducer(js jetstream.JetStream, cfg NATSProducerConfig) *NATSProducer {
	return &NATSProducer{js: js, cfg: cfg}
}

// Publish encodes the event as JSON and publishes to the shard-derived subject.
// Shard is computed via ShardOfMember (keygroup.go) so the routing convention
// lives in one place — change there once, both producer and consumer follow.
func (p *NATSProducer) Publish(ctx context.Context, event *behaviorModel.BehaviorEvent) error {
	shard := ShardOfMember(event.MemberID, p.cfg.NumShards)
	subject := fmt.Sprintf("%s.%d.%s", p.cfg.SubjectPrefix, shard, event.MemberID)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("nats producer: marshal: %w", err)
	}
	if _, err := p.js.Publish(ctx, subject, data); err != nil {
		return fmt.Errorf("nats producer: publish: %w", err)
	}
	return nil
}

// Close is a no-op for NATS — the underlying nats.Conn is owned by the caller
// (typical pattern: cmd/main connects, defers nc.Close, hands js to producer).
func (p *NATSProducer) Close() error { return nil }
