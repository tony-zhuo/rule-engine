package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// kafkaBrokers is defined in kafka_testcontainer_test.go (3-tier lookup:
// -short → env → testcontainers → skip).

func kafkaTestSetup(t *testing.T, brokers []string) (topic string, cleanup func()) {
	t.Helper()
	topic = fmt.Sprintf("rule-engine-test-%d", time.Now().UnixNano())
	admClient, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("kgo client (admin): %v", err)
	}
	adm := kadm.NewClient(admClient)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := adm.CreateTopics(ctx, 1, 1, nil, topic); err != nil {
		admClient.Close()
		t.Fatalf("create topic: %v", err)
	}
	return topic, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = adm.DeleteTopics(ctx, topic)
		admClient.Close()
	}
}

func publishWithdrawKafka(t *testing.T, brokers []string, topic, id, member string, amount float64, at time.Time) {
	t.Helper()
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("producer client: %v", err)
	}
	defer client.Close()
	ev := behaviorModel.BehaviorEvent{
		EventID:    id,
		MemberID:   member,
		Behavior:   behaviorModel.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": amount},
		OccurredAt: at,
	}
	data, _ := json.Marshal(ev)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.ProduceSync(ctx, &kgo.Record{Topic: topic, Value: data}).FirstErr(); err != nil {
		t.Fatalf("produce: %v", err)
	}
}

// TestKafka_EndToEnd is the Kafka mirror of TestRun_EndToEnd: publish events,
// run the Kafka consumer, verify the in-memory state reflects every event.
// Same workload, same assertions — proving the engine produces identical
// results across MQ backends (the basis of Task L shadow comparison).
func TestKafka_EndToEnd(t *testing.T) {
	brokers := kafkaBrokers(t)
	topic, cleanup := kafkaTestSetup(t, brokers)
	defer cleanup()

	base := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	publishWithdrawKafka(t, brokers, topic, "e0", "u1", 4000, base)
	publishWithdrawKafka(t, brokers, topic, "e1", "u1", 4000, base.Add(time.Minute))
	publishWithdrawKafka(t, brokers, topic, "e2", "u1", 4000, base.Add(2*time.Minute))

	consumerClient, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatalf("consumer client: %v", err)
	}
	defer consumerClient.Close()

	core := NewCore(0, buildWithdrawRuleSet(t))
	consumer := NewKafkaConsumer(core, consumerClient, KafkaConfig{
		Topic:          topic,
		Partition:      0,
		MaxPollRecords: 100,
	})

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	// Records produced at offsets 0,1,2 — lastSeq should reach 2.
	waitFor(t, func() bool { return core.lastSeq.Load() >= 2 }, 10*time.Second)
	cancelRun()
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	ms := core.State.Members["u1"]
	if ms == nil {
		t.Fatal("u1 has no state — events not processed")
	}
	var totalCount uint64
	var totalSum float64
	for _, b := range ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets {
		totalCount += b.Count
		totalSum += b.Sums["amount"]
	}
	if totalCount != 3 || totalSum != 12000 {
		t.Fatalf("got count=%d sum=%v, want count=3 sum=12000", totalCount, totalSum)
	}
}

// TestKafka_CrashRecovery mirrors TestRun_CrashRecovery on Kafka: one Core
// processes + stops, a brand-new Core replays from offset 0 and reaches
// identical state — proving the offset-self-management recipe (snapshot's
// LastSeq, not consumer group commits) works across backends.
func TestKafka_CrashRecovery(t *testing.T) {
	brokers := kafkaBrokers(t)
	topic, cleanup := kafkaTestSetup(t, brokers)
	defer cleanup()

	base := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	publishWithdrawKafka(t, brokers, topic, "e0", "u1", 4000, base)
	publishWithdrawKafka(t, brokers, topic, "e1", "u1", 4000, base.Add(time.Minute))
	publishWithdrawKafka(t, brokers, topic, "e2", "u1", 4000, base.Add(2*time.Minute))

	cfg := KafkaConfig{
		Topic:          topic,
		Partition:      0,
		MaxPollRecords: 100,
	}

	runUntilCaughtUp := func() *Core {
		client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		defer client.Close()
		c := NewCore(0, buildWithdrawRuleSet(t))
		consumer := NewKafkaConsumer(c, client, cfg)
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- consumer.Run(runCtx) }()
		waitFor(t, func() bool { return c.lastSeq.Load() >= 2 }, 10*time.Second)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("run error: %v", err)
		}
		return c
	}

	_ = runUntilCaughtUp()          // first Core processes, then "crashes"
	recovered := runUntilCaughtUp() // fresh Core replays the log from offset 0

	ms := recovered.State.Members["u1"]
	if ms == nil {
		t.Fatal("recovered core lost u1 state")
	}
	var totalCount uint64
	var totalSum float64
	for _, b := range ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets {
		totalCount += b.Count
		totalSum += b.Sums["amount"]
	}
	if totalCount != 3 || totalSum != 12000 {
		t.Fatalf("after recovery got count=%d sum=%v, want count=3 sum=12000 (double-count or loss)", totalCount, totalSum)
	}
}
