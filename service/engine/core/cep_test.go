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
