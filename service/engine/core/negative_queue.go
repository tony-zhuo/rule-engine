package core

import (
	"container/heap"
	"time"

	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
)

// negDeadlineEntry is a pending negative-pattern deadline. The heap orders by
// Deadline so the shard's watermark sweep can pop in O(log n) per fired match.
// Entries can become stale (the progress was aborted by a matching event before
// the deadline arrived) — drain() validates each popped entry against live
// state before emitting, so staleness is harmless.
type negDeadlineEntry struct {
	Deadline   time.Time
	MemberID   string
	ProgressID string
}

// negDeadlineHeap is a min-heap over Deadline. Owned by Core; mutated only by
// the shard's main goroutine, so no locking — same single-writer model as the
// rest of the engine state.
type negDeadlineHeap []negDeadlineEntry

func (h negDeadlineHeap) Len() int            { return len(h) }
func (h negDeadlineHeap) Less(i, j int) bool  { return h[i].Deadline.Before(h[j].Deadline) }
func (h negDeadlineHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *negDeadlineHeap) Push(x any)         { *h = append(*h, x.(negDeadlineEntry)) }
func (h *negDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// pushNegativeDeadline registers a (deadline, member, progress) triple. Called
// by cep.go whenever a progress steps into a negative state.
func (c *Core) pushNegativeDeadline(deadline time.Time, memberID, progressID string) {
	heap.Push(&c.negDeadlines, negDeadlineEntry{
		Deadline:   deadline,
		MemberID:   memberID,
		ProgressID: progressID,
	})
}

// drainNegativeMatches pops every entry whose deadline has been passed by the
// watermark and emits a MatchResult for each progress that's still live and
// still parked on its negative state. Stale entries (progress aborted or moved
// on) are silently discarded.
//
// "watermark passed" means strictly later — an entry whose deadline equals the
// watermark hasn't been "completed" yet under event-time semantics.
func (c *Core) drainNegativeMatches(watermark time.Time) []*cepModel.MatchResult {
	var emitted []*cepModel.MatchResult
	for c.negDeadlines.Len() > 0 {
		top := c.negDeadlines[0]
		if !watermark.After(top.Deadline) {
			break
		}
		heap.Pop(&c.negDeadlines)

		ms, ok := c.State.Members[top.MemberID]
		if !ok {
			continue
		}
		p, ok := ms.Progresses[top.ProgressID]
		if !ok {
			continue // already aborted / cleaned up
		}
		// Verify the progress is still parked on its negative state. The
		// deadline is the bookkeeping marker we set when entering the negative
		// state; if it's been zeroed (state moved on) the entry is stale.
		if p.NegativeDeadline.IsZero() || !p.NegativeDeadline.Equal(top.Deadline) {
			continue
		}
		cp := c.patternByID(p.PatternID)
		if cp == nil {
			delete(ms.Progresses, top.ProgressID)
			continue
		}
		emitted = append(emitted, &cepModel.MatchResult{
			PatternID:   cp.pattern.ID,
			PatternName: cp.pattern.Name,
			MemberID:    p.MemberID,
			Variables:   p.Variables,
			MatchedAt:   p.NegativeDeadline,
		})
		delete(ms.Progresses, top.ProgressID)
	}
	return emitted
}
