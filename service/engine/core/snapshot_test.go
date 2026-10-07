package core

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// TestSnapshot_RoundTrip proves the state survives gob serialize → deserialize
// byte-for-byte: a crashed shard restored from a snapshot equals the original.
func TestSnapshot_RoundTrip(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	c1 := NewCore(0, rs)
	c1.ProcessEvent(withdraw("e0", "alice", 3000, base))
	c1.ProcessEvent(withdraw("e1", "bob", 7000, base.Add(time.Minute)))
	c1.ProcessEvent(withdraw("e2", "alice", 4500, base.Add(2*time.Minute)))

	data, err := c1.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Simulate a crash + restart: fresh Core restores from the snapshot bytes.
	c2 := NewCore(0, rs)
	if _, err := c2.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !reflect.DeepEqual(c1.State, c2.State) {
		t.Fatal("restored state does not equal original")
	}
	if !c1.Watermark().Equal(c2.Watermark()) {
		t.Fatalf("restored watermark %v != original %v", c2.Watermark(), c1.Watermark())
	}
}

// TestSnapshot_ReplaySafety proves the central recovery property: restoring a
// snapshot and replaying events that overlap the snapshot reconstructs exactly
// the same state as processing every event once — because the dedup set lives
// inside the snapshot, replayed-but-already-applied events are skipped.
func TestSnapshot_ReplaySafety(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	e0 := withdraw("e0", "u1", 3000, base)
	e1 := withdraw("e1", "u1", 4000, base.Add(time.Minute))
	e2 := withdraw("e2", "u1", 5000, base.Add(2*time.Minute))

	// Original: process e0, e1, then snapshot (state reflects e0+e1).
	orig := NewCore(0, rs)
	orig.ProcessEvent(e0)
	orig.ProcessEvent(e1)
	data, err := orig.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Crash + restart: restore, then replay e0, e1 (overlap with snapshot) and e2.
	recovered := NewCore(0, rs)
	if _, err := recovered.Restore(data); err != nil {
		t.Fatalf("restore: %v", err)
	}
	recovered.BeginReplay()
	recovered.ProcessEvent(e0) // already in snapshot → deduped
	recovered.ProcessEvent(e1) // already in snapshot → deduped
	recovered.ProcessEvent(e2) // new
	recovered.EndReplay()

	// Reference: a core that processed all three events exactly once.
	reference := NewCore(0, rs)
	reference.ProcessEvent(e0)
	reference.ProcessEvent(e1)
	reference.ProcessEvent(e2)

	if !reflect.DeepEqual(recovered.State, reference.State) {
		t.Fatal("replay double-counted overlapping events — state diverged from single-pass")
	}

	// Concretely: u1's bucket sum must be 3000+4000+5000 = 12000, not inflated.
	bucket := recovered.State.member("u1").Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[alignBucket(base)]
	// e0 is in bucket(base); e1,e2 are in later buckets, so this bucket holds only e0.
	if bucket.Sums["amount"] != 3000 {
		t.Fatalf("bucket(base) sum = %v, want 3000 (no double count)", bucket.Sums["amount"])
	}
}

// TestSnapshot_ReplaySuppressesSideEffects proves the late sink does not fire
// while replaying — re-emitting side effects for already-processed events is wrong.
func TestSnapshot_ReplaySuppressesSideEffects(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	var lateCalls int
	c := NewCore(0, rs,
		WithAllowedLateness(0),
		WithLateSink(func(*behaviorModel.BehaviorEvent) { lateCalls++ }),
	)

	c.BeginReplay()
	c.ProcessEvent(withdraw("fresh", "u1", 1000, base.Add(time.Minute))) // advances watermark
	c.ProcessEvent(withdraw("late", "u1", 1000, base))                    // late, but replaying
	c.EndReplay()

	if lateCalls != 0 {
		t.Fatalf("late sink fired %d times during replay, want 0", lateCalls)
	}
}

// TestSnapshot_EncodeWhileWriting is the gap #20 property: the frozen view can
// be encoded on another goroutine while the main loop keeps writing to the same
// members, and what lands in the snapshot is exactly the state at freeze time.
// Run with -race to catch any write path that bypasses copy-on-write.
func TestSnapshot_EncodeWhileWriting(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	event := func(i int) *behaviorModel.BehaviorEvent {
		return withdraw(fmt.Sprintf("e%d", i), fmt.Sprintf("u%d", i%50), 100, base.Add(time.Duration(i)*time.Second))
	}

	live := NewCore(0, rs)
	reference := NewCore(0, rs)
	for i := 0; i < 500; i++ {
		live.ProcessEvent(event(i))
		reference.ProcessEvent(event(i))
	}
	live.lastSeq.Store(500)

	snap := live.beginSnapshot()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result)
	go func() {
		data, err := snap.encode()
		done <- result{data, err}
	}()
	for i := 500; i < 5000; i++ {
		live.ProcessEvent(event(i))
		live.lastSeq.Store(uint64(i + 1))
	}
	res := <-done
	live.endSnapshot()
	if res.err != nil {
		t.Fatalf("encode: %v", res.err)
	}

	recovered := NewCore(0, rs)
	seq, err := recovered.Restore(res.data)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if seq != 500 {
		t.Fatalf("restored LastSeq = %d, want 500 (value at freeze)", seq)
	}
	if !reflect.DeepEqual(recovered.State, reference.State) {
		t.Fatal("snapshot does not equal the state at freeze time")
	}
	if !recovered.Watermark().Equal(reference.Watermark()) {
		t.Fatalf("restored watermark %v != %v at freeze", recovered.Watermark(), reference.Watermark())
	}
}

func restoreFile(t *testing.T, rs *ruleModel.CompiledRuleSet, path string) (*Core, uint64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	c := NewCore(0, rs)
	seq, err := c.Restore(data)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return c, seq
}

func TestAsyncSnapshotter_BackgroundThenDrain(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "shard.snap")

	c := NewCore(0, rs)
	s := newAsyncSnapshotter(path, time.Nanosecond)
	c.ProcessEvent(withdraw("e0", "u1", 1000, base))
	c.lastSeq.Store(1)
	s.tick(c) // interval elapsed → background snapshot of seq 1 starts
	if s.done == nil {
		t.Fatal("tick did not start a background snapshot")
	}

	deadline := time.Now().Add(5 * time.Second)
	for i := 2; s.done != nil; i++ {
		if time.Now().After(deadline) {
			t.Fatal("background snapshot never completed")
		}
		c.ProcessEvent(withdraw(fmt.Sprintf("e%d", i), "u1", 1000, base.Add(time.Duration(i)*time.Second)))
		c.lastSeq.Store(uint64(i))
		s.interval = time.Hour // only observe completion, don't start another
		s.tick(c)
	}
	if c.frozenEpoch != 0 {
		t.Fatal("state still frozen after the background snapshot completed")
	}
	if _, seq := restoreFile(t, rs, path); seq != 1 {
		t.Fatalf("background snapshot LastSeq = %d, want 1 (value at freeze)", seq)
	}

	if err := s.drain(c); err != nil {
		t.Fatalf("drain: %v", err)
	}
	recovered, seq := restoreFile(t, rs, path)
	if seq != c.lastSeq.Load() {
		t.Fatalf("final snapshot LastSeq = %d, want %d", seq, c.lastSeq.Load())
	}
	// Compare against a restore of the live state rather than c.State itself:
	// members cloned during the background snapshot carry a non-zero COW
	// version, which is bookkeeping, not state, and is never serialized.
	liveData, err := c.Snapshot()
	if err != nil {
		t.Fatalf("snapshot live: %v", err)
	}
	want := NewCore(0, rs)
	if _, err := want.Restore(liveData); err != nil {
		t.Fatalf("restore live: %v", err)
	}
	if !reflect.DeepEqual(recovered.State, want.State) {
		t.Fatal("final snapshot does not equal live state")
	}
}

func TestAsyncSnapshotter_Gating(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	tests := []struct {
		name      string
		path      string
		interval  time.Duration
		wantStart bool
	}{
		{"disabled without path", "", time.Nanosecond, false},
		{"disabled without interval", "x.snap", 0, false},
		{"interval not yet elapsed", "x.snap", time.Hour, false},
		{"interval elapsed", "x.snap", time.Nanosecond, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.path
			if path != "" {
				path = filepath.Join(t.TempDir(), path)
			}
			c := NewCore(0, rs)
			s := newAsyncSnapshotter(path, tt.interval)
			s.tick(c)
			if started := s.done != nil; started != tt.wantStart {
				t.Fatalf("started = %v, want %v", started, tt.wantStart)
			}
			if err := s.drain(c); err != nil {
				t.Fatalf("drain: %v", err)
			}
			if c.frozenEpoch != 0 {
				t.Fatal("state still frozen after drain")
			}
		})
	}
}
