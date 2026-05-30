package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/twmb/franz-go/pkg/kgo"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// KafkaProducerConfig configures the Kafka producer (franz-go).
type KafkaProducerConfig struct {
	Topic     string // e.g. "rule-events"
	NumShards int    // shard count, for routing: shard → Kafka partition
}

// KafkaProducer publishes BehaviorEvents to Kafka. Partition is computed via
// ShardOfMember (keygroup.go) so the engine's shard ↔ partition mapping is
// deterministic and lives in one place — change the routing convention once,
// both producer and consumer follow.
type KafkaProducer struct {
	client *kgo.Client
	cfg    KafkaProducerConfig
}

var _ EventProducer = (*KafkaProducer)(nil)

// NewKafkaProducer wires a franz-go client with this shard layout. The caller
// owns the *kgo.Client (symmetric with NATSProducer's *jetstream.JetStream).
// Typical client options for production use:
//
//	kgo.NewClient(
//	    kgo.SeedBrokers(brokers...),
//	    kgo.RequiredAcks(kgo.AllISRAcks()),  // durability
//	    kgo.RecordRetries(...),
//	)
func NewKafkaProducer(client *kgo.Client, cfg KafkaProducerConfig) *KafkaProducer {
	return &KafkaProducer{client: client, cfg: cfg}
}

// Publish encodes the event as JSON and produces synchronously to the
// shard-derived partition. We set both Partition (deterministic routing) and
// Key (member_id, useful for any downstream partitioner-aware tools).
func (p *KafkaProducer) Publish(ctx context.Context, event *behaviorModel.BehaviorEvent) error {
	shard := ShardOfMember(event.MemberID, p.cfg.NumShards)
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("kafka producer: marshal: %w", err)
	}
	record := &kgo.Record{
		Topic:     p.cfg.Topic,
		Partition: int32(shard),
		Key:       []byte(event.MemberID),
		Value:     data,
	}
	if err := p.client.ProduceSync(ctx, record).FirstErr(); err != nil {
		return fmt.Errorf("kafka producer: publish: %w", err)
	}
	return nil
}

// Close is a no-op — the underlying *kgo.Client is owned by the caller
// (symmetric with NATSProducer.Close, same lifecycle contract).
func (p *KafkaProducer) Close() error { return nil }
