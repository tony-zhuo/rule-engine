package core

import (
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// behaviorIs builds a condition node "behavior == <name>".
func behaviorIs(name string) ruleModel.RuleNode {
	return ruleModel.RuleNode{
		Type:     ruleModel.NodeCondition,
		Field:    "behavior",
		Operator: "=",
		Value:    name,
	}
}

// fraudPattern: login_failed → login_failed → login_success, each within 5 min.
func fraudPattern() cepModel.CEPPattern {
	w := &ruleModel.TimeWindow{Value: 5, Unit: "minutes"}
	return cepModel.CEPPattern{
		ID:   "fraud",
		Name: "FraudAttempt",
		States: []cepModel.PatternState{
			{Name: "fail1", Condition: behaviorIs("login_failed"), MaxWait: w},
			{Name: "fail2", Condition: behaviorIs("login_failed"), MaxWait: w},
			{Name: "success", Condition: behaviorIs("login_success"), MaxWait: w},
		},
	}
}

func behaviorEvent(id, member, behavior string, at time.Time) *behaviorModel.BehaviorEvent {
	return &behaviorModel.BehaviorEvent{
		EventID:    id,
		MemberID:   member,
		Behavior:   behaviorModel.BehaviorType(behavior),
		Fields:     map[string]any{},
		OccurredAt: at,
	}
}

func newCEPCore(t *testing.T) *Core {
	t.Helper()
	c := NewCore(0, &ruleModel.CompiledRuleSet{}) // no rules; CEP only
	if err := c.AddPattern(fraudPattern()); err != nil {
		t.Fatalf("add pattern: %v", err)
	}
	return c
}

// TestCEP_FraudSequence proves a multi-step pattern fires exactly when the full
// ordered sequence completes within the windows.
func TestCEP_FraudSequence(t *testing.T) {
	c := newCEPCore(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	steps := []struct {
		ev        *behaviorModel.BehaviorEvent
		wantMatch bool
	}{
		{behaviorEvent("e0", "u1", "login_failed", base), false},
		{behaviorEvent("e1", "u1", "login_failed", base.Add(1*time.Minute)), false},
		{behaviorEvent("e2", "u1", "login_success", base.Add(2*time.Minute)), true},
	}
	for i, s := range steps {
		res := c.ProcessEvent(s.ev)
		got := len(res.MatchedPatterns) == 1 && res.MatchedPatterns[0].PatternID == "fraud"
		if got != s.wantMatch {
			t.Fatalf("step %d: got match=%v want %v (patterns=%v)", i, got, s.wantMatch, res.MatchedPatterns)
		}
	}
	// The completed progress (started by e0) is removed on match.
	progresses := c.State.Members["u1"].Progresses
	if _, exists := progresses["fraud|u1|e0"]; exists {
		t.Fatal("completed progress fraud|u1|e0 should have been removed")
	}
	// But a parallel progress legitimately remains: the 2nd login_failed (e1) was
	// both "step 2 of the first progress" and "the start of a new progress". That
	// second machine is still in-flight, waiting for its own next login_failed.
	if _, exists := progresses["fraud|u1|e1"]; !exists {
		t.Fatal("expected the parallel progress started by e1 to remain in-flight")
	}
	if n := len(progresses); n != 1 {
		t.Fatalf("expected exactly 1 in-flight progress, have %d", n)
	}
}

// TestCEP_WindowExpiry proves a sequence that exceeds the per-state window does
// not complete: the last step arrives after MaxWait from the progress start.
func TestCEP_WindowExpiry(t *testing.T) {
	c := newCEPCore(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	c.ProcessEvent(behaviorEvent("e0", "u1", "login_failed", base))
	c.ProcessEvent(behaviorEvent("e1", "u1", "login_failed", base.Add(1*time.Minute)))
	// success 6 minutes after start — beyond the 5-minute MaxWait → no match.
	res := c.ProcessEvent(behaviorEvent("e2", "u1", "login_success", base.Add(6*time.Minute)))
	if len(res.MatchedPatterns) != 0 {
		t.Fatalf("expected no match after window expiry, got %v", res.MatchedPatterns)
	}
}

// TestCEP_Idempotent proves redelivering the completing event does not produce a
// second match, and that deterministic progress IDs make replay safe.
func TestCEP_Idempotent(t *testing.T) {
	c := newCEPCore(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	c.ProcessEvent(behaviorEvent("e0", "u1", "login_failed", base))
	c.ProcessEvent(behaviorEvent("e1", "u1", "login_failed", base.Add(1*time.Minute)))
	first := c.ProcessEvent(behaviorEvent("e2", "u1", "login_success", base.Add(2*time.Minute)))
	if len(first.MatchedPatterns) != 1 {
		t.Fatalf("expected 1 match, got %d", len(first.MatchedPatterns))
	}
	// Redeliver the same completing event: progress is already gone, and the
	// start-dedup key prevents re-opening, so no second match.
	again := c.ProcessEvent(behaviorEvent("e2", "u1", "login_success", base.Add(2*time.Minute)))
	if len(again.MatchedPatterns) != 0 {
		t.Fatalf("redelivery should not re-fire, got %v", again.MatchedPatterns)
	}
}

// missedVerifyPattern: a canonical negative pattern — "user logged in but did
// NOT verify within 5 minutes". The second state is IsNegative+MaxWait, which
// is the supported terminal-negative shape (mirrors Flink notFollowedBy.within).
func missedVerifyPattern() cepModel.CEPPattern {
	return cepModel.CEPPattern{
		ID:   "missed_verify",
		Name: "LoginWithoutVerify",
		States: []cepModel.PatternState{
			{Name: "login", Condition: behaviorIs("login")},
			{
				Name:       "no_verify",
				Condition:  behaviorIs("verify"),
				MaxWait:    &ruleModel.TimeWindow{Value: 5, Unit: "minutes"},
				IsNegative: true,
			},
		},
	}
}

func newNegativeCore(t *testing.T) *Core {
	t.Helper()
	// allowedLateness=0 lets the watermark equal the latest event time exactly,
	// making it easy to reason about when the negative deadline is "passed".
	c := NewCore(0, &ruleModel.CompiledRuleSet{}, WithAllowedLateness(0))
	if err := c.AddPattern(missedVerifyPattern()); err != nil {
		t.Fatalf("add pattern: %v", err)
	}
	return c
}

// TestCEP_NegativeFiresOnDeadline proves a negative pattern emits exactly when
// the watermark passes the deadline without an aborting event arriving. We don't
// fire it directly — we have to wait for SOMETHING to push the watermark past
// the deadline, which is the whole point of timer-driven negative semantics.
func TestCEP_NegativeFiresOnDeadline(t *testing.T) {
	c := newNegativeCore(t)
	base := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)

	// 1. login arrives, progress parked on negative state with deadline = base+5min.
	if res := c.ProcessEvent(behaviorEvent("e0", "alice", "login", base)); len(res.MatchedPatterns) != 0 {
		t.Fatalf("login alone must not fire, got %v", res.MatchedPatterns)
	}
	if c.negDeadlines.Len() != 1 {
		t.Fatalf("expected 1 pending negative deadline, got %d", c.negDeadlines.Len())
	}

	// 2. unrelated event 3 min in — watermark = base+3m, still inside the window.
	if res := c.ProcessEvent(behaviorEvent("e1", "bob", "browse", base.Add(3*time.Minute))); len(res.MatchedPatterns) != 0 {
		t.Fatalf("watermark still inside window must not fire, got %v", res.MatchedPatterns)
	}
	if c.negDeadlines.Len() != 1 {
		t.Fatalf("deadline should still be pending, got %d", c.negDeadlines.Len())
	}

	// 3. unrelated event 6 min in — watermark = base+6m > deadline → fire.
	res := c.ProcessEvent(behaviorEvent("e2", "bob", "browse", base.Add(6*time.Minute)))
	if got := len(res.MatchedPatterns); got != 1 {
		t.Fatalf("expected 1 negative match, got %d (%v)", got, res.MatchedPatterns)
	}
	if res.MatchedPatterns[0].PatternID != "missed_verify" || res.MatchedPatterns[0].MemberID != "alice" {
		t.Fatalf("wrong match: %+v", res.MatchedPatterns[0])
	}
	if !res.MatchedPatterns[0].MatchedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("MatchedAt should be the deadline (%v), got %v",
			base.Add(5*time.Minute), res.MatchedPatterns[0].MatchedAt)
	}
	if c.negDeadlines.Len() != 0 {
		t.Fatalf("deadline heap should be drained, got %d", c.negDeadlines.Len())
	}
	if _, exists := c.State.Members["alice"].Progresses["missed_verify|alice|e0"]; exists {
		t.Fatal("progress should be removed after firing")
	}
}

// TestCEP_NegativeAbortedByMatch proves a verify arriving within the window
// silently aborts — no match fires, the progress is removed, and any later
// deadline tick does nothing.
func TestCEP_NegativeAbortedByMatch(t *testing.T) {
	c := newNegativeCore(t)
	base := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)

	c.ProcessEvent(behaviorEvent("e0", "alice", "login", base))
	if res := c.ProcessEvent(behaviorEvent("e1", "alice", "verify", base.Add(2*time.Minute))); len(res.MatchedPatterns) != 0 {
		t.Fatalf("verify within window aborts, must not emit a match, got %v", res.MatchedPatterns)
	}
	if _, exists := c.State.Members["alice"].Progresses["missed_verify|alice|e0"]; exists {
		t.Fatal("progress should be deleted after the aborting event")
	}
	// Heap entry is still there (stale), but draining past the deadline must not fire.
	res := c.ProcessEvent(behaviorEvent("e2", "bob", "browse", base.Add(10*time.Minute)))
	if len(res.MatchedPatterns) != 0 {
		t.Fatalf("stale heap entry must not produce a match, got %v", res.MatchedPatterns)
	}
}

// TestCEP_NegativeBoundary nails down the exact "watermark passed deadline"
// semantic: deadline == watermark does NOT fire (window not yet completed);
// watermark > deadline does.
func TestCEP_NegativeBoundary(t *testing.T) {
	c := newNegativeCore(t)
	base := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)

	c.ProcessEvent(behaviorEvent("e0", "alice", "login", base))

	// Exactly at the deadline (watermark = base+5m, deadline = base+5m).
	if res := c.ProcessEvent(behaviorEvent("e1", "bob", "browse", base.Add(5*time.Minute))); len(res.MatchedPatterns) != 0 {
		t.Fatalf("watermark exactly at deadline must NOT fire (strict-after semantics), got %v", res.MatchedPatterns)
	}

	// One nanosecond past the deadline: now it fires.
	res := c.ProcessEvent(behaviorEvent("e2", "bob", "browse", base.Add(5*time.Minute+time.Nanosecond)))
	if len(res.MatchedPatterns) != 1 {
		t.Fatalf("watermark past deadline by 1ns should fire, got %d", len(res.MatchedPatterns))
	}
}

// TestCEP_AddPattern_RejectsMidSequenceNegative proves the terminal-only
// validation — mid-sequence negatives are explicitly out of scope for v1.
func TestCEP_AddPattern_RejectsMidSequenceNegative(t *testing.T) {
	c := NewCore(0, &ruleModel.CompiledRuleSet{})
	bad := cepModel.CEPPattern{
		ID:   "bad",
		Name: "MidNegative",
		States: []cepModel.PatternState{
			{Name: "a", Condition: behaviorIs("a")},
			{Name: "no_b", Condition: behaviorIs("b"), MaxWait: &ruleModel.TimeWindow{Value: 1, Unit: "minutes"}, IsNegative: true},
			{Name: "c", Condition: behaviorIs("c")},
		},
	}
	if err := c.AddPattern(bad); err == nil {
		t.Fatal("expected error for non-terminal negative state")
	}
}
