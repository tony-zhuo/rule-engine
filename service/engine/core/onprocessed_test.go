package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// latencyCollector is a thread-safe OnProcessed sink for tests. The callback
// fires on the consumer goroutine while assertions run on the test goroutine.
type latencyCollector struct {
	mu   sync.Mutex
	durs []time.Duration
}

func (lc *latencyCollector) add(d time.Duration) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.durs = append(lc.durs, d)
}

func (lc *latencyCollector) snapshot() []time.Duration {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	out := make([]time.Duration, len(lc.durs))
	copy(out, lc.durs)
	return out
}

// TestOnProcessed_LiveEvents verifies that OnProcessed fires once per live
// event with a sane end-to-end latency (now - OccurredAt): non-negative and
// far below the test timeout.
func TestOnProcessed_LiveEvents(t *testing.T) {
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
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     "rule-events",
		Subjects: []string{"rule.events.>"},
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}

	lc := &latencyCollector{}
	core := NewCore(0, buildWithdrawRuleSet(t))
	consumer := NewNATSConsumer(core, js, NATSConfig{
		StreamName:    "rule-events",
		Subjects:      []string{"rule.events.>"},
		FilterSubject: "rule.events.0.>",
		MaxAckPending: 100,
		OnProcessed:   lc.add,
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	// Wait for Run to register its consumer — that happens after it reads
	// stream.Info, so events published from here on are live, not replay.
	waitFor(t, func() bool {
		si, ierr := stream.Info(ctx)
		return ierr == nil && si.State.Consumers >= 1
	}, 5*time.Second)

	// Stream was empty at start → no replay; everything published now is live.
	// OccurredAt = publish wall time, so latency = consume delay (small, >= 0).
	for i, id := range []string{"e0", "e1", "e2"} {
		publishWithdraw(t, js, id, "u1", float64(100*(i+1)), time.Now())
	}
	waitFor(t, func() bool { return core.lastSeq.Load() >= 3 }, 5*time.Second)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	durs := lc.snapshot()
	if len(durs) != 3 {
		t.Fatalf("got %d OnProcessed calls, want 3", len(durs))
	}
	for i, d := range durs {
		if d < 0 || d > 5*time.Second {
			t.Fatalf("latency[%d] = %v, want within (0, 5s)", i, d)
		}
	}
}

// TestOnProcessed_SkippedDuringReplay verifies the callback stays silent for
// replayed events: their OccurredAt is in the past, so "now - OccurredAt" is
// replay lag, not end-to-end latency — reporting it would poison benchmarks.
func TestOnProcessed_SkippedDuringReplay(t *testing.T) {
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

	// Two events already in the log before the consumer starts → replayed.
	publishWithdraw(t, js, "old0", "u1", 100, time.Now().Add(-time.Minute))
	publishWithdraw(t, js, "old1", "u1", 100, time.Now().Add(-time.Minute))

	lc := &latencyCollector{}
	core := NewCore(0, buildWithdrawRuleSet(t))
	consumer := NewNATSConsumer(core, js, NATSConfig{
		StreamName:    "rule-events",
		Subjects:      []string{"rule.events.>"},
		FilterSubject: "rule.events.0.>",
		MaxAckPending: 100,
		OnProcessed:   lc.add,
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- consumer.Run(runCtx) }()

	// Wait until the two old events are replayed, then publish one live event.
	waitFor(t, func() bool { return core.lastSeq.Load() >= 2 }, 5*time.Second)
	publishWithdraw(t, js, "live0", "u1", 100, time.Now())
	waitFor(t, func() bool { return core.lastSeq.Load() >= 3 }, 5*time.Second)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	durs := lc.snapshot()
	if len(durs) != 1 {
		t.Fatalf("got %d OnProcessed calls, want 1 (replayed events must not fire)", len(durs))
	}
}
