package core

import "time"

// The watermark is the shard's completeness clock: a claim that no event with
// OccurredAt < watermark will be accepted for a real-time decision. It trails
// the largest event time seen by allowedLateness, giving out-of-order events
// that much slack before they are treated as late (plan §時間語意與 Watermark).
//
// watermark is an atomic.Int64 (unix nanos) only because read-only observers
// (metrics, debug endpoints) on other goroutines read it; the single shard
// goroutine is the sole writer, so advance needs no compare-and-swap.

// isLate reports whether an event is behind the watermark and thus too late for
// a real-time decision. Classified against the watermark as it stands *before*
// this event advances it.
func (c *Core) isLate(t time.Time) bool {
	return t.UnixNano() < c.watermark.Load()
}

// advanceWatermark moves the watermark forward to t-allowedLateness if that is
// newer. It only ever moves forward (monotonic), so feeding an older on-time
// event never rewinds it.
func (c *Core) advanceWatermark(t time.Time) {
	candidate := t.Add(-c.allowedLateness).UnixNano()
	if candidate > c.watermark.Load() {
		c.watermark.Store(candidate)
	}
}

// Watermark returns the current watermark as a time. Exposed for metrics/debug.
func (c *Core) Watermark() time.Time {
	return time.Unix(0, c.watermark.Load())
}

// laterOf returns the later of two times — used to track LastSeenAt as the max
// event time seen, which is correct even when events arrive out of order.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
