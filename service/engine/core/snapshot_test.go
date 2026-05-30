package core

import (
	"reflect"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
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
	bucket := recovered.State.Members["u1"].Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[alignBucket(base)]
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
