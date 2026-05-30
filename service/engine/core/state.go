package core

import (
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

// ShardState is the entire in-memory state owned by one shard. It is mutated by
// a single goroutine (the shard's main loop), which is why nothing here is
// guarded by a lock — the single-writer principle replaces mutual exclusion.
//
// Fields are exported so the whole tree can be serialized with encoding/gob in
// the snapshot step.
type ShardState struct {
	Members map[string]*MemberState // memberID → state
}

func NewShardState() *ShardState {
	return &ShardState{Members: make(map[string]*MemberState)}
}

// getOrCreateMember returns the member's state, creating an empty one on first
// sight. Safe without locking because only the shard's goroutine calls it.
func (s *ShardState) getOrCreateMember(memberID string) *MemberState {
	ms, ok := s.Members[memberID]
	if !ok {
		ms = newMemberState(memberID)
		s.Members[memberID] = ms
	}
	return ms
}

// MemberState holds everything the engine knows about one member: their
// per-behavior time-bucketed aggregations and any in-flight CEP progresses.
type MemberState struct {
	MemberID     string
	Aggregations map[behaviorModel.BehaviorType]*BehaviorAgg // per-behavior aggregations
	Progresses   map[string]*cepModel.PatternProgress        // progressID → CEP state
	LastSeenAt   time.Time                                   // event-time of last event; LRU eviction reference
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
// buckets instead of scanning every raw event (the Redis sorted-set approach).
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
