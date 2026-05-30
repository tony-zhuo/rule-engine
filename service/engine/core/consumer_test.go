package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// startEmbeddedNATS runs an in-process NATS server with JetStream — a real
// end-to-end test with no external dependency or Docker.
func startEmbeddedNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Port:      -1, // random free port
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	return s
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func publishWithdraw(t *testing.T, js jetstream.JetStream, id, member string, amount float64, at time.Time) {
	t.Helper()
	ev := behaviorModel.BehaviorEvent{
		EventID:    id,
		MemberID:   member,
		Behavior:   behaviorModel.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": amount},
		OccurredAt: at,
	}
	data, _ := json.Marshal(ev)
	if _, err := js.Publish(context.Background(), "rule.events.0."+member, data); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// TestRun_EndToEnd publishes events to JetStream, runs the Core consumer, and
// verifies the in-memory state reflects every event — proving the NATS ↔ Core
// wiring works end to end (decode → ProcessEvent → ack → lastSeq).
func TestRun_EndToEnd(t *testing.T) {
	s := startEmbeddedNATS(t)
	defer s.Shutdown()

	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "rule-events",
		Subjects: []string{"rule.events.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	// Three withdrawals of 4000 for u1, one per minute → cumulative 12000.
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	publishWithdraw(t, js, "e0", "u1", 4000, base)
	publishWithdraw(t, js, "e1", "u1", 4000, base.Add(time.Minute))
	publishWithdraw(t, js, "e2", "u1", 4000, base.Add(2*time.Minute))

	core := NewCore(0, buildWithdrawRuleSet(t))
	consumer := NewNATSConsumer(core, js, NATSConfig{
		StreamName:    "rule-events",
		Subjects:      []string{"rule.events.>"},
		FilterSubject: "rule.events.0.>",
		MaxAckPending: 100,
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	// Wait until all three events have been processed (lastSeq is atomic-safe).
	waitFor(t, func() bool { return core.lastSeq.Load() >= 3 }, 5*time.Second)

	// Stop the consumer so we can read state without a concurrent writer.
	cancel()
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

// TestRun_CrashRecovery proves recovery over real NATS by pure log replay: one
// Core processes the events and stops ("crash"); a brand-new Core then replays
// the persisted JetStream log from the start and reconstructs identical state —
// no events lost, none double-counted. (Snapshot+replay correctness is covered
// separately in snapshot_test.go; here the source of truth is the NATS log.)
func TestRun_CrashRecovery(t *testing.T) {
	s := startEmbeddedNATS(t)
	defer s.Shutdown()

	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)

	ctx := context.Background()
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "rule-events",
		Subjects: []string{"rule.events.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}

	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	publishWithdraw(t, js, "e0", "u1", 4000, base)
	publishWithdraw(t, js, "e1", "u1", 4000, base.Add(time.Minute))
	publishWithdraw(t, js, "e2", "u1", 4000, base.Add(2*time.Minute))

	cfg := NATSConfig{
		StreamName:    "rule-events",
		Subjects:      []string{"rule.events.>"},
		FilterSubject: "rule.events.0.>",
		MaxAckPending: 100,
	}

	runUntilCaughtUp := func() *Core {
		core := NewCore(0, buildWithdrawRuleSet(t))
		consumer := NewNATSConsumer(core, js, cfg)
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- consumer.Run(runCtx) }()
		waitFor(t, func() bool { return core.lastSeq.Load() >= 3 }, 5*time.Second)
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("run error: %v", err)
		}
		return core
	}

	_ = runUntilCaughtUp()       // first Core processes, then "crashes"
	recovered := runUntilCaughtUp() // fresh Core replays the log from seq 1

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
