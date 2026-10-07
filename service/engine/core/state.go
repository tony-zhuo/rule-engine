package core

import (
	"maps"
	"slices"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

// ShardState is the entire in-memory state owned by one shard. It is mutated by
// a single goroutine (the shard's main loop), which is why nothing here is
// guarded by a lock — the single-writer principle replaces mutual exclusion.
//
// Members are bucketed by key group. Besides matching the snapshot / rescale
// unit, this bounds the copy-on-write cost: replacing a member with its clone is
// itself a map write, so the map must be cloned too, and only the one key group
// being written (~NumMembers/NumKeyGroups entries) is copied, not the whole shard.
//
// Copy-on-write invariants (see Core.memberForWrite):
//   - Every mutation goes through memberForWrite; member() is read-only.
//   - Never keep a *MemberState or *PatternProgress across events — after a
//     clone the old pointer belongs to the in-flight snapshot.
type ShardState struct {
	keyGroups [NumKeyGroups]*keyGroupState // nil until the key group's first write
}

type keyGroupState struct {
	Members map[string]*MemberState // memberID → state
	version uint64                  // epoch this map was created or cloned in
}

func NewShardState() *ShardState {
	return &ShardState{}
}

// member returns the member's state for reading, or nil if never seen.
func (s *ShardState) member(memberID string) *MemberState {
	kg := s.keyGroups[KeyGroupOf(memberID)]
	if kg == nil {
		return nil
	}
	return kg.Members[memberID]
}

// forEachMember visits every member, key group by key group.
func (s *ShardState) forEachMember(fn func(*MemberState)) {
	for _, kg := range s.keyGroups {
		if kg == nil {
			continue
		}
		for _, ms := range kg.Members {
			fn(ms)
		}
	}
}

// memberForWrite returns the member's state for mutation, creating it on first
// sight. While a snapshot is in flight (c.frozenEpoch != 0), any key group map or
// member older than the freeze is still shared with the snapshot, so it is
// cloned first and the clone takes its place in the live state. Each object is
// cloned at most once per freeze: the clone is stamped with the current epoch.
func (c *Core) memberForWrite(memberID string) *MemberState {
	id := KeyGroupOf(memberID)
	kg := c.State.keyGroups[id]
	switch {
	case kg == nil:
		kg = &keyGroupState{Members: make(map[string]*MemberState), version: c.epoch}
		c.State.keyGroups[id] = kg
	case c.frozenEpoch != 0 && kg.version < c.frozenEpoch:
		kg = &keyGroupState{Members: maps.Clone(kg.Members), version: c.epoch}
		c.State.keyGroups[id] = kg
	}

	ms, ok := kg.Members[memberID]
	switch {
	case !ok:
		ms = newMemberState(memberID)
		ms.version = c.epoch
		kg.Members[memberID] = ms
	case c.frozenEpoch != 0 && ms.version < c.frozenEpoch:
		ms = ms.clone()
		ms.version = c.epoch
		kg.Members[memberID] = ms
	}
	return ms
}

// freezeState starts a copy-on-write epoch and returns the frozen view: a copy
// of the key group pointer array, safe to read from another goroutine until
// unfreezeState. Called by the main goroutine only.
func (c *Core) freezeState() ShardState {
	c.epoch++
	c.frozenEpoch = c.epoch
	return *c.State
}

// unfreezeState ends the copy-on-write epoch once the snapshot no longer reads
// the frozen view. Called by the main goroutine only.
func (c *Core) unfreezeState() {
	c.frozenEpoch = 0
}

// MemberState holds everything the engine knows about one member: their
// per-behavior time-bucketed aggregations and any in-flight CEP progresses.
type MemberState struct {
	MemberID     string
	Aggregations map[behaviorModel.BehaviorType]*BehaviorAgg // per-behavior aggregations
	Progresses   map[string]*cepModel.PatternProgress        // progressID → CEP state
	LastSeenAt   time.Time                                   // event-time of last event; LRU eviction reference
	version      uint64                                      // epoch this member was created or cloned in (not serialized)
}

func newMemberState(memberID string) *MemberState {
	return &MemberState{
		MemberID:     memberID,
		Aggregations: make(map[behaviorModel.BehaviorType]*BehaviorAgg),
		Progresses:   make(map[string]*cepModel.PatternProgress),
	}
}

// BehaviorAgg holds the time-bucketed aggregations for one behavior of one
// member. Bucketing pre-aggregates events so a window query sums a handful of
// buckets instead of scanning every raw event in the window.
type BehaviorAgg struct {
	Buckets map[int64]*BucketData // aligned bucket start (unix secs) → data
}

func newBehaviorAgg() *BehaviorAgg {
	return &BehaviorAgg{Buckets: make(map[int64]*BucketData)}
}

// BucketData is the pre-aggregated state for one time bucket. ProcessedEventIDs
// lives here, alongside the counters it guards, so a snapshot of the bucket
// carries its own dedup set — the basis of replay safety (plan §冪等性保證).
type BucketData struct {
	Count             uint64
	Sums              map[string]float64  // numeric field → running sum
	Maxs              map[string]float64  // numeric field → running max
	Mins              map[string]float64  // numeric field → running min
	ProcessedEventIDs map[string]struct{} // event_id seen in this bucket (idempotency)
}

func newBucketData() *BucketData {
	return &BucketData{
		Sums:              make(map[string]float64),
		Maxs:              make(map[string]float64),
		Mins:              make(map[string]float64),
		ProcessedEventIDs: make(map[string]struct{}),
	}
}

// clone deep-copies the member so the copy can be mutated while a snapshot
// still reads the original. Variables values are only string / float64
// (snapshot.go registers no other types), so a shallow map copy is enough there.
func (ms *MemberState) clone() *MemberState {
	out := &MemberState{
		MemberID:     ms.MemberID,
		Aggregations: make(map[behaviorModel.BehaviorType]*BehaviorAgg, len(ms.Aggregations)),
		Progresses:   make(map[string]*cepModel.PatternProgress, len(ms.Progresses)),
		LastSeenAt:   ms.LastSeenAt,
	}
	for k, agg := range ms.Aggregations {
		out.Aggregations[k] = agg.clone()
	}
	for k, p := range ms.Progresses {
		cp := *p
		cp.Variables = maps.Clone(p.Variables)
		cp.ProcessedEvents = slices.Clone(p.ProcessedEvents)
		out.Progresses[k] = &cp
	}
	return out
}

func (a *BehaviorAgg) clone() *BehaviorAgg {
	out := &BehaviorAgg{Buckets: make(map[int64]*BucketData, len(a.Buckets))}
	for ts, b := range a.Buckets {
		out.Buckets[ts] = b.clone()
	}
	return out
}

func (b *BucketData) clone() *BucketData {
	return &BucketData{
		Count:             b.Count,
		Sums:              maps.Clone(b.Sums),
		Maxs:              maps.Clone(b.Maxs),
		Mins:              maps.Clone(b.Mins),
		ProcessedEventIDs: maps.Clone(b.ProcessedEventIDs),
	}
}
