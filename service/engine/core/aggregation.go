package core

import (
	"strings"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// bucketSize is the time-bucket granularity. Window queries are accurate to this
// resolution: a 5-minute window sums five 1-minute buckets, so the window edge
// can be off by up to one bucket. This trades time precision for memory (no raw
// events stored) and O(buckets) instead of O(events) reads — see plan §4.
const bucketSize = time.Minute

// alignBucket truncates an event time to its bucket's start, used as the bucket key.
func alignBucket(t time.Time) int64 {
	return t.Truncate(bucketSize).Unix()
}

// updateAggregations folds one event into the member's in-memory buckets, then
// prunes buckets that can no longer fall inside any rule window. It is idempotent:
// an event_id already counted in its bucket is skipped, which is what makes
// replay (and at-least-once redelivery) safe.
//
// numericFields is the event's fields pre-converted to float64. maxWindow is the
// longest window across all active rules; buckets older than that are dropped.
func updateAggregations(
	ms *MemberState,
	behavior behaviorModel.BehaviorType,
	eventID string,
	occurredAt time.Time,
	numericFields map[string]float64,
	maxWindow time.Duration,
) {
	agg, ok := ms.Aggregations[behavior]
	if !ok {
		agg = newBehaviorAgg()
		ms.Aggregations[behavior] = agg
	}

	bucketTs := alignBucket(occurredAt)
	bucket, ok := agg.Buckets[bucketTs]
	if !ok {
		bucket = newBucketData()
		agg.Buckets[bucketTs] = bucket
	}

	// Idempotency: the same event_id must only be counted once in this bucket.
	// Correctness depends on eventID satisfying the Event Identity Contract —
	// see docs/in-memory-rule-engine-plan.md §Event Identity Contract.
	// Violating that contract (e.g., regenerating ID at the broker, recycling
	// IDs, content-hash collisions) silently inflates or merges counts here.
	if _, dup := bucket.ProcessedEventIDs[eventID]; dup {
		return
	}
	bucket.ProcessedEventIDs[eventID] = struct{}{}

	bucket.Count++
	for field, value := range numericFields {
		bucket.Sums[field] += value
		if cur, seen := bucket.Maxs[field]; !seen || value > cur {
			bucket.Maxs[field] = value
		}
		if cur, seen := bucket.Mins[field]; !seen || value < cur {
			bucket.Mins[field] = value
		}
	}

	pruneExpiredBuckets(agg, occurredAt, maxWindow)
}

// pruneExpiredBuckets drops buckets that ended before the oldest point any rule
// window could reach (occurredAt - maxWindow), bounding memory per member.
func pruneExpiredBuckets(agg *BehaviorAgg, occurredAt time.Time, maxWindow time.Duration) {
	cutoff := alignBucket(occurredAt.Add(-maxWindow))
	for ts := range agg.Buckets {
		if ts < cutoff {
			delete(agg.Buckets, ts)
		}
	}
}

// numericFieldsOf converts an event's raw fields to float64, keeping only those
// that are numeric. Non-numeric fields (strings, bools) are simply ignored —
// they are not aggregatable, and COUNT needs no field at all.
func numericFieldsOf(fields map[string]any) map[string]float64 {
	out := make(map[string]float64, len(fields))
	for k, v := range fields {
		if f, ok := toFloat64(v); ok {
			out[k] = f
		}
	}
	return out
}

// parseAggregateField splits "Behavior:AGG:field" (or "Behavior:AGG" for COUNT)
// into its parts, matching the format used by the rule compiler and Redis store.
func parseAggregateField(field string) (behavior behaviorModel.BehaviorType, agg, fieldPath string) {
	parts := strings.SplitN(field, ":", 3)
	if len(parts) < 2 {
		return "", "", ""
	}
	behavior = behaviorModel.BehaviorType(parts[0])
	agg = strings.ToUpper(parts[1])
	if len(parts) == 3 {
		fieldPath = parts[2]
	}
	return behavior, agg, fieldPath
}

// computeBucketAggregation answers one aggregate key (e.g. "Trade:SUM:amount"
// over 5 minutes) from the member's in-memory buckets — the read path that
// replaces a Redis ZRANGEBYSCORE + parse. It sums the buckets whose start falls
// within [now - window, now].
func computeBucketAggregation(ms *MemberState, key ruleModel.AggregateKey, now time.Time) float64 {
	behavior, agg, fieldPath := parseAggregateField(key.Field)

	behaviorAgg, ok := ms.Aggregations[behavior]
	if !ok {
		return 0
	}

	// Window is [now-window, now]. Both bounds matter: the lower bound excludes
	// expired buckets, and the *upper* bound excludes buckets whose timestamp is
	// after this event — which can exist when events arrive out of order. Without
	// the upper bound, a future event's bucket would leak into an earlier event's
	// window (see Task E).
	var since int64
	if key.Window != nil {
		since = alignBucket(now.Add(-key.Window.Duration()))
	}
	upper := alignBucket(now)

	var (
		totalCount uint64
		totalSum   float64
		maxVal     float64
		minVal     float64
		haveMax    bool
		haveMin    bool
	)
	for ts, bucket := range behaviorAgg.Buckets {
		if ts < since || ts > upper {
			continue
		}
		totalCount += bucket.Count
		totalSum += bucket.Sums[fieldPath]
		if v, seen := bucket.Maxs[fieldPath]; seen && (!haveMax || v > maxVal) {
			maxVal, haveMax = v, true
		}
		if v, seen := bucket.Mins[fieldPath]; seen && (!haveMin || v < minVal) {
			minVal, haveMin = v, true
		}
	}

	switch agg {
	case "COUNT":
		return float64(totalCount)
	case "SUM":
		return totalSum
	case "AVG":
		if totalCount == 0 {
			return 0
		}
		return totalSum / float64(totalCount)
	case "MAX":
		return maxVal
	case "MIN":
		return minVal
	default:
		return 0
	}
}

// toFloat64 best-effort converts a value to float64. Mirrors the rule engine's
// numeric coercion so aggregation matches comparison semantics.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
