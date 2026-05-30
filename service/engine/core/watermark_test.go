package core

import (
	"reflect"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// TestProcessEvent_OutOfOrder_NoFutureLeak proves the window upper bound: an
// event that occurred later but arrived earlier must not leak into the window
// of an earlier event. Without the upper bound, B below would see A's amount
// and the 10000 rule would wrongly fire.
func TestProcessEvent_OutOfOrder_NoFutureLeak(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	// Large lateness so the earlier-arriving-later event B is still "on time"
	// and actually gets evaluated (isolating the window-bound behavior).
	c := NewCore(0, rs, WithAllowedLateness(time.Hour))
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	a := withdraw("A", "u1", 8000, base.Add(5*time.Minute)) // 10:05, arrives first
	b := withdraw("B", "u1", 8000, base)                    // 10:00, arrives second

	if got := c.ProcessEvent(a).MatchedRules; len(got) != 0 {
		t.Fatalf("A alone (8000) should not fire, got %v", got)
	}
	// B's window is [09:55, 10:00]; A's 10:05 bucket is in the future relative to
	// B and must be excluded. So B sees only 8000 → no fire. If the upper bound
	// were missing, B would see 8000+8000=16000 > 10000 and fire.
	if got := c.ProcessEvent(b).MatchedRules; len(got) != 0 {
		t.Fatalf("B must not see future event A; expected no fire, got %v", got)
	}
}

// TestProcessEvent_OutOfOrder_StateEqual proves aggregation state is independent
// of arrival order: feeding the same events in order and shuffled yields
// byte-identical state. Late events still update aggregation (commutative), so
// nothing is lost — only the real-time firing decision differs.
func TestProcessEvent_OutOfOrder_StateEqual(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	ordered := []*behaviorModel.BehaviorEvent{
		withdraw("e0", "alice", 3000, base),
		withdraw("e1", "bob", 7000, base.Add(1*time.Minute)),
		withdraw("e2", "alice", 4500, base.Add(2*time.Minute)),
		withdraw("e3", "alice", 5000, base.Add(3*time.Minute)),
	}
	shuffled := []*behaviorModel.BehaviorEvent{ordered[2], ordered[0], ordered[3], ordered[1]}

	// allowedLateness 0 → in the shuffled run several events land behind the
	// watermark and are classified late, yet must still be applied to state.
	run := func(stream []*behaviorModel.BehaviorEvent) *ShardState {
		c := NewCore(0, rs, WithAllowedLateness(0))
		for _, ev := range stream {
			c.ProcessEvent(ev)
		}
		return c.State
	}

	if !reflect.DeepEqual(run(ordered), run(shuffled)) {
		t.Fatal("aggregation state depends on arrival order — not order-independent")
	}
}

// TestProcessEvent_LateEvent_SideOutput proves a late event is routed to the
// side output and does not fire a real-time decision.
func TestProcessEvent_LateEvent_SideOutput(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	var sideOutput []*behaviorModel.BehaviorEvent
	c := NewCore(0, rs,
		WithAllowedLateness(0),
		WithLateSink(func(e *behaviorModel.BehaviorEvent) { sideOutput = append(sideOutput, e) }),
	)

	c.ProcessEvent(withdraw("fresh", "u1", 1000, base.Add(time.Minute))) // sets watermark to 10:01
	late := withdraw("late", "u1", 1000, base)                            // 10:00 < watermark → late
	res := c.ProcessEvent(late)
	if len(res.MatchedRules) != 0 || len(res.MatchedPatterns) != 0 {
		t.Fatalf("late event should not fire, got %+v", res)
	}

	if len(sideOutput) != 1 || sideOutput[0].EventID != "late" {
		t.Fatalf("late event not routed to side output: %v", sideOutput)
	}
	// But it was still applied to aggregation (commutative completeness).
	bucket := c.State.Members["u1"].Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[alignBucket(base)]
	if bucket == nil || bucket.Count != 1 {
		t.Fatalf("late event must still update aggregation state")
	}
}
