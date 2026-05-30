package core

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/twmb/franz-go/pkg/kgo"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// TestShadow_BothBackendsAgree is the headline MQ-pluggable proof: publish the
// SAME event stream to NATS and Kafka, run a Core on each, and require their
// final in-memory state to be byte-identical (reflect.DeepEqual on ShardState).
//
// This is the structural evidence that the engine is genuinely MQ-agnostic —
// not just "we have two backends" but "they actually produce the same answers".
// On a résumé/interview this is the line: "implemented both backends behind one
// consumer interface and validated identical state via shadow traffic."
//
// Env-gated: needs KAFKA_BROKERS for the Kafka side; embedded NATS auto-starts.
func TestShadow_BothBackendsAgree(t *testing.T) {
	brokers := kafkaBrokers(t) // skips if KAFKA_BROKERS unset

	// --- NATS side: embedded server, single shard, stream rule.events.> ---
	natsSrv := startEmbeddedNATS(t)
	defer natsSrv.Shutdown()
	nc, err := nats.Connect(natsSrv.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "rule-events",
		Subjects: []string{"rule.events.>"},
	}); err != nil {
		t.Fatalf("nats create stream: %v", err)
	}
	natsProducer := NewNATSProducer(js, NATSProducerConfig{
		SubjectPrefix: "rule.events",
		NumShards:     1,
	})

	// --- Kafka side: external broker, per-test topic, single partition ---
	topic, cleanup := kafkaTestSetup(t, brokers)
	defer cleanup()
	kafkaClient, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("kafka client: %v", err)
	}
	defer kafkaClient.Close()
	kafkaProducer := NewKafkaProducer(kafkaClient, KafkaProducerConfig{
		Topic:     topic,
		NumShards: 1,
	})

	// --- Same event stream into both ---
	base := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	stream := []*behaviorModel.BehaviorEvent{
		makeEvent("e0", "alice", 3000, base),
		makeEvent("e1", "bob", 7000, base.Add(1*time.Minute)),
		makeEvent("e2", "alice", 4500, base.Add(2*time.Minute)),
		makeEvent("e3", "alice", 5000, base.Add(3*time.Minute)),
		makeEvent("e4", "bob", 2000, base.Add(4*time.Minute)),
	}
	for _, ev := range stream {
		if err := natsProducer.Publish(ctx, ev); err != nil {
			t.Fatalf("nats publish: %v", err)
		}
		if err := kafkaProducer.Publish(ctx, ev); err != nil {
			t.Fatalf("kafka publish: %v", err)
		}
	}

	// --- Run a Core on each backend ---
	rs := buildWithdrawRuleSet(t)

	natsCore := NewCore(0, rs)
	natsConsumer := NewNATSConsumer(natsCore, js, NATSConfig{
		StreamName:    "rule-events",
		Subjects:      []string{"rule.events.>"},
		FilterSubject: "rule.events.0.>",
		MaxAckPending: 100,
	})

	kafkaConsumerClient, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("kafka consumer client: %v", err)
	}
	defer kafkaConsumerClient.Close()
	kafkaCore := NewCore(0, rs)
	kafkaConsumer := NewKafkaConsumer(kafkaCore, kafkaConsumerClient, KafkaConfig{
		Topic:          topic,
		Partition:      0,
		MaxPollRecords: 100,
	})

	natsCtx, cancelNats := context.WithCancel(ctx)
	kafkaCtx, cancelKafka := context.WithCancel(ctx)
	natsDone := make(chan error, 1)
	kafkaDone := make(chan error, 1)
	go func() { natsDone <- natsConsumer.Run(natsCtx) }()
	go func() { kafkaDone <- kafkaConsumer.Run(kafkaCtx) }()

	// NATS sequences start at 1 (5 events → lastSeq 5); Kafka offsets at 0 (→ 4).
	want := func(lastSeq, threshold uint64) bool { return lastSeq >= threshold }
	waitFor(t, func() bool {
		return want(natsCore.lastSeq.Load(), 5) && want(kafkaCore.lastSeq.Load(), 4)
	}, 15*time.Second)

	cancelNats()
	cancelKafka()
	if err := <-natsDone; err != nil {
		t.Fatalf("nats consumer error: %v", err)
	}
	if err := <-kafkaDone; err != nil {
		t.Fatalf("kafka consumer error: %v", err)
	}

	// --- The whole point: state must be identical across MQ backends ---
	if !reflect.DeepEqual(natsCore.State, kafkaCore.State) {
		t.Fatalf("backends disagree on ShardState:\n  NATS  = %#v\n  Kafka = %#v",
			natsCore.State, kafkaCore.State)
	}

	// Sanity: state actually contains the data (not both equal because both empty).
	alice := natsCore.State.Members["alice"]
	if alice == nil {
		t.Fatal("alice missing from shadow state — neither backend processed events?")
	}
	var aliceSum float64
	for _, b := range alice.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets {
		aliceSum += b.Sums["amount"]
	}
	if aliceSum != 12500 {
		t.Fatalf("alice sum = %v, want 12500 (3000+4500+5000)", aliceSum)
	}
}

// makeEvent is the shared event builder used across shadow assertions; keeps
// the test focused on the comparison, not on event construction noise.
func makeEvent(id, member string, amount float64, at time.Time) *behaviorModel.BehaviorEvent {
	return &behaviorModel.BehaviorEvent{
		EventID:    id,
		MemberID:   member,
		Behavior:   behaviorModel.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": amount},
		OccurredAt: at,
	}
}
