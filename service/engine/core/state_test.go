package core

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

// sameKeyGroupMembers returns two distinct member IDs that hash to the same key
// group, so tests can observe the kg-level map clone independently of the
// member-level clone.
func sameKeyGroupMembers(t *testing.T) (string, string) {
	t.Helper()
	seen := make(map[KeyGroupID]string)
	for i := 0; i < 100_000; i++ {
		id := fmt.Sprintf("m%d", i)
		kg := KeyGroupOf(id)
		if other, ok := seen[kg]; ok {
			return other, id
		}
		seen[kg] = id
	}
	t.Fatal("no two members share a key group")
	return "", ""
}

func bucketSum(ms *MemberState, at time.Time) float64 {
	return ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[alignBucket(at)].Sums["amount"]
}

func TestMemberForWrite_CreatesAndReuses(t *testing.T) {
	c := NewCore(0, buildWithdrawRuleSet(t))

	if c.State.member("u1") != nil {
		t.Fatal("member exists before first write")
	}
	ms := c.memberForWrite("u1")
	if ms == nil || ms.MemberID != "u1" {
		t.Fatalf("memberForWrite returned %+v", ms)
	}
	if got := c.State.member("u1"); got != ms {
		t.Fatal("member() does not return the written member")
	}
	if again := c.memberForWrite("u1"); again != ms {
		t.Fatal("memberForWrite cloned with no snapshot in flight")
	}
}

// TestCopyOnWrite_FrozenViewIsStable covers each kind of write a shard makes
// while a snapshot is in flight: the frozen view must keep the pre-freeze state
// while the live state moves on.
func TestCopyOnWrite_FrozenViewIsStable(t *testing.T) {
	base := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		write func(c *Core)
		check func(t *testing.T, frozen, live *ShardState)
	}{
		{
			name:  "update existing member",
			write: func(c *Core) { c.ProcessEvent(withdraw("e1", "u1", 500, base)) },
			check: func(t *testing.T, frozen, live *ShardState) {
				if got := bucketSum(frozen.member("u1"), base); got != 1000 {
					t.Fatalf("frozen sum = %v, want 1000", got)
				}
				if got := bucketSum(live.member("u1"), base); got != 1500 {
					t.Fatalf("live sum = %v, want 1500", got)
				}
			},
		},
		{
			name:  "create new member",
			write: func(c *Core) { c.ProcessEvent(withdraw("e1", "u2", 500, base)) },
			check: func(t *testing.T, frozen, live *ShardState) {
				if frozen.member("u2") != nil {
					t.Fatal("member created after freeze leaked into frozen view")
				}
				if live.member("u2") == nil {
					t.Fatal("new member missing from live state")
				}
			},
		},
		{
			name:  "delete progress",
			write: func(c *Core) { delete(c.memberForWrite("u1").Progresses, "p1") },
			check: func(t *testing.T, frozen, live *ShardState) {
				if _, ok := frozen.member("u1").Progresses["p1"]; !ok {
					t.Fatal("progress deleted after freeze vanished from frozen view")
				}
				if _, ok := live.member("u1").Progresses["p1"]; ok {
					t.Fatal("progress still present in live state")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCore(0, buildWithdrawRuleSet(t))
			c.ProcessEvent(withdraw("e0", "u1", 1000, base))
			c.memberForWrite("u1").Progresses["p1"] = &cepModel.PatternProgress{ID: "p1", MemberID: "u1"}

			frozen := c.freezeState()
			tt.write(c)
			tt.check(t, &frozen, c.State)
		})
	}
}

func TestCopyOnWrite_ClonesOncePerFreeze(t *testing.T) {
	c := NewCore(0, buildWithdrawRuleSet(t))
	a, b := sameKeyGroupMembers(t)
	origA := c.memberForWrite(a)
	c.memberForWrite(b)
	kg := KeyGroupOf(a)
	origKG := c.State.keyGroups[kg]

	frozen := c.freezeState()

	cloneA := c.memberForWrite(a)
	if cloneA == origA {
		t.Fatal("first write after freeze did not clone the member")
	}
	if frozen.member(a) != origA {
		t.Fatal("frozen view no longer points at the original member")
	}
	if c.memberForWrite(a) != cloneA {
		t.Fatal("second write in the same freeze cloned the member again")
	}

	liveKG := c.State.keyGroups[kg]
	if liveKG == origKG {
		t.Fatal("first write after freeze did not clone the key group")
	}
	c.memberForWrite(b)
	if c.State.keyGroups[kg] != liveKG {
		t.Fatal("second write to the same key group cloned it again")
	}
}

func TestCopyOnWrite_NoCloneAfterUnfreeze(t *testing.T) {
	c := NewCore(0, buildWithdrawRuleSet(t))
	orig := c.memberForWrite("u1")

	c.freezeState()
	c.unfreezeState()

	if c.memberForWrite("u1") != orig {
		t.Fatal("member cloned after the snapshot was released")
	}
}

func TestMemberState_CloneIsDeep(t *testing.T) {
	build := func() *MemberState {
		ms := newMemberState("u1")
		ms.LastSeenAt = time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
		agg := newBehaviorAgg()
		bucket := newBucketData()
		bucket.Count = 1
		bucket.Sums["amount"] = 10
		bucket.Maxs["amount"] = 10
		bucket.Mins["amount"] = 10
		bucket.ProcessedEventIDs["e0"] = struct{}{}
		agg.Buckets[100] = bucket
		ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw] = agg
		ms.Progresses["p1"] = &cepModel.PatternProgress{
			ID:              "p1",
			Variables:       map[string]any{"amount": 10.0},
			ProcessedEvents: []string{"e0"},
		}
		return ms
	}

	orig, pristine := build(), build()
	cl := orig.clone()

	tests := []struct {
		name   string
		mutate func(ms *MemberState)
	}{
		{"scalar field", func(ms *MemberState) { ms.LastSeenAt = time.Time{} }},
		{"aggregations map", func(ms *MemberState) { ms.Aggregations["other"] = newBehaviorAgg() }},
		{"buckets map", func(ms *MemberState) {
			ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[200] = newBucketData()
		}},
		{"bucket counters", func(ms *MemberState) {
			b := ms.Aggregations[behaviorModel.BehaviorCryptoWithdraw].Buckets[100]
			b.Count++
			b.Sums["amount"] = 99
			b.Maxs["amount"] = 99
			b.Mins["amount"] = 0
			b.ProcessedEventIDs["e1"] = struct{}{}
		}},
		{"progresses map", func(ms *MemberState) { ms.Progresses["p2"] = &cepModel.PatternProgress{} }},
		{"progress fields", func(ms *MemberState) {
			p := ms.Progresses["p1"]
			p.CurrentStep = 3
			p.Variables["amount"] = 99.0
			p.ProcessedEvents[0] = "changed"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate(cl)
			if !reflect.DeepEqual(orig, pristine) {
				t.Fatalf("mutating the clone's %s changed the original", tt.name)
			}
		})
	}
}
