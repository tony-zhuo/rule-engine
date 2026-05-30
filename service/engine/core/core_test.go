package core

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

// buildWithdrawRuleSet builds a one-rule set: "sum of CryptoWithdraw.amount over
// the last 5 minutes > 10000". Reuses the real rule compiler — no mocking.
func buildWithdrawRuleSet(t *testing.T) *ruleModel.CompiledRuleSet {
	t.Helper()
	node := ruleModel.RuleNode{
		Type:     ruleModel.NodeCondition,
		Field:    "CryptoWithdraw:SUM:amount",
		Operator: ">",
		Value:    float64(10000),
		Window:   &ruleModel.TimeWindow{Value: 5, Unit: "minutes"},
	}
	eval, err := ruleUsecase.Compile(node)
	if err != nil {
		t.Fatalf("compile rule: %v", err)
	}
	strategies := []ruleModel.CompiledStrategy{{
		ID:            1,
		Name:          "big_withdraw",
		Eval:          eval,
		AggregateKeys: ruleModel.CollectAggregateKeys(node),
	}}
	allKeys := ruleUsecase.CollectUniqueAggregateKeys(strategies)
	return &ruleModel.CompiledRuleSet{
		Strategies: strategies,
		MaxWindow:  ruleUsecase.MaxWindowFromKeys(allKeys),
	}
}

func withdraw(id, member string, amount float64, at time.Time) *behaviorModel.BehaviorEvent {
	return &behaviorModel.BehaviorEvent{
		EventID:    id,
		MemberID:   member,
		Behavior:   behaviorModel.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": amount},
		OccurredAt: at,
	}
}

// TestProcessEvent_RuleFires proves the full pipeline (aggregate → evaluate)
// works end to end: the rule fires only once the windowed sum crosses 10000.
func TestProcessEvent_RuleFires(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	c := NewCore(0, rs)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	// Three withdrawals of 4000, one per minute → cumulative 4000, 8000, 12000.
	wantMatch := []bool{false, false, true} // crosses 10000 on the 3rd
	for i := 0; i < 3; i++ {
		ev := withdraw(fmt.Sprintf("e%d", i), "u1", 4000, base.Add(time.Duration(i)*time.Minute))
		res := c.ProcessEvent(ev)
		fired := len(res.MatchedRules) == 1 && res.MatchedRules[0].Name == "big_withdraw"
		if fired != wantMatch[i] {
			t.Fatalf("event %d: got fired=%v, want %v (matched=%v)", i, fired, wantMatch[i], res.MatchedRules)
		}
	}
}

// TestProcessEvent_Idempotent proves a redelivered event (same event_id) is
// counted only once — the basis of at-least-once + replay safety.
func TestProcessEvent_Idempotent(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	c := NewCore(0, rs)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	ev := withdraw("dup", "u1", 5000, base)
	c.ProcessEvent(ev)
	c.ProcessEvent(ev) // redelivery of the exact same event

	bucket := c.State.Members["u1"].Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[alignBucket(base)]
	if bucket.Count != 1 {
		t.Fatalf("count after duplicate = %d, want 1", bucket.Count)
	}
	if bucket.Sums["amount"] != 5000 {
		t.Fatalf("sum after duplicate = %v, want 5000", bucket.Sums["amount"])
	}
}

// TestProcessEvent_Deterministic proves replaying the same event stream produces
// byte-identical state — the property that makes snapshot+replay recovery safe.
func TestProcessEvent_Deterministic(t *testing.T) {
	rs := buildWithdrawRuleSet(t)
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	stream := []*behaviorModel.BehaviorEvent{
		withdraw("e0", "alice", 3000, base),
		withdraw("e1", "bob", 7000, base.Add(30*time.Second)),
		withdraw("e2", "alice", 4500, base.Add(1*time.Minute)),
		withdraw("e3", "alice", 5000, base.Add(2*time.Minute)),
		withdraw("e4", "bob", 2000, base.Add(3*time.Minute)),
	}

	run := func() *ShardState {
		c := NewCore(0, rs)
		for _, ev := range stream {
			c.ProcessEvent(ev)
		}
		return c.State
	}

	s1, s2 := run(), run()
	if !reflect.DeepEqual(s1, s2) {
		t.Fatal("state diverged across two identical runs — not deterministic")
	}
}
