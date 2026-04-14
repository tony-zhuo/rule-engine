package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

var _ model.BehaviorEventStoreInterface = (*BehaviorEventStore)(nil)

const (
	redisEventsPrefix = "rule_engine:events:"
	memberSeparator   = '|'
)

// BehaviorEventStore stores behavioral events in Redis Sorted Sets for
// real-time aggregation queries, replacing PostgreSQL on the hot path.
//
// Key schema:   rule_engine:events:{member_id}:{behavior}
// Score:        occurred_at as Unix milliseconds
// Member:       "event_id|val0|val1|..." where val0..valN are the numeric
//               field values in schema order. Missing / non-numeric values
//               are written as empty between delimiters (parsed as 0).
//
// The pipe-separated format is zero-allocation to parse, eliminating the
// GC pressure that JSON unmarshal caused on the hot path.
type BehaviorEventStore struct {
	client *goredis.Client
}

func NewBehaviorEventStore(client *goredis.Client) *BehaviorEventStore {
	return &BehaviorEventStore{client: client}
}

// storeEventScript atomically: ZADD NX + ZREMRANGEBYSCORE (prune) + EXPIRE.
// KEYS[1] = sorted set key
// ARGV[1] = score (occurred_at millis)
// ARGV[2] = member value (encoded)
// ARGV[3] = cutoff score for pruning
// ARGV[4] = TTL in seconds
// Returns 1 if newly added, 0 if duplicate.
var storeEventScript = goredis.NewScript(`
local added = redis.call('ZADD', KEYS[1], 'NX', ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[3])
redis.call('EXPIRE', KEYS[1], ARGV[4])
return added
`)

// encodeMember builds "event_id|val0|val1|..." from an event and schema.
// Values are formatted with strconv.FormatFloat('f', -1, 64) for a canonical
// form (no scientific notation, shortest representation).
func encodeMember(event *model.BehaviorEvent, schema *model.FieldSchema) string {
	if schema == nil || len(schema.Fields) == 0 {
		return event.EventID
	}
	// Pre-size the builder: event_id + (sep + ~20 chars per field).
	var b strings.Builder
	b.Grow(len(event.EventID) + len(schema.Fields)*21)
	b.WriteString(event.EventID)
	for _, name := range schema.Fields {
		b.WriteByte(memberSeparator)
		if v, ok := event.Fields[name]; ok {
			if f, ok := toFloat64(v); ok {
				b.WriteString(strconv.FormatFloat(f, 'f', -1, 64))
			}
		}
	}
	return b.String()
}

// parseFieldAt extracts the float64 value at the given schema position from a
// pipe-separated member string. Position -1 means the caller wants COUNT
// and no parse is needed. Returns (value, ok) where ok is false if the slot
// is missing, empty, or unparseable.
//
// This function is zero-allocation: strings.Cut returns sub-slices, and
// strconv.ParseFloat doesn't allocate for valid numeric strings.
func parseFieldAt(member string, pos int) (float64, bool) {
	if pos < 0 {
		return 0, false
	}
	// Skip the event_id at position 0 and then `pos` separator-delimited fields.
	s := member
	for i := 0; i <= pos; i++ {
		_, rest, found := strings.Cut(s, string(memberSeparator))
		if !found {
			return 0, false
		}
		s = rest
	}
	// s now starts at the wanted field; take up to the next separator.
	valStr, _, _ := strings.Cut(s, string(memberSeparator))
	if valStr == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func (s *BehaviorEventStore) StoreEvent(ctx context.Context, event *model.BehaviorEvent, schemas map[model.BehaviorType]*model.FieldSchema, maxWindow time.Duration) (bool, error) {
	key := s.eventKey(event.MemberID, string(event.Behavior))
	score := float64(event.OccurredAt.UnixMilli())
	member := encodeMember(event, schemas[event.Behavior])

	cutoff := time.Now().Add(-maxWindow).UnixMilli()
	ttlSeconds := int(maxWindow.Seconds())
	if ttlSeconds <= 0 {
		ttlSeconds = 24 * 60 * 60
	}

	result, err := storeEventScript.Run(ctx, s.client, []string{key},
		score, member, cutoff, ttlSeconds,
	).Int()
	if err != nil {
		return false, fmt.Errorf("behavior event store: store event: %w", err)
	}
	return result == 1, nil
}

// StoreAndAggregate combines StoreEvent + BatchAggregate into a single Redis
// pipeline, reducing 2 round-trips to 1. The store Lua script and all
// ZRANGEBYSCORE commands are sent together in one pipeline.Exec() call.
func (s *BehaviorEventStore) StoreAndAggregate(ctx context.Context, event *model.BehaviorEvent, schemas map[model.BehaviorType]*model.FieldSchema, conds []model.AggregateCond, maxWindow time.Duration) (map[string]float64, error) {
	storeKey := s.eventKey(event.MemberID, string(event.Behavior))
	score := float64(event.OccurredAt.UnixMilli())
	member := encodeMember(event, schemas[event.Behavior])

	cutoff := time.Now().Add(-maxWindow).UnixMilli()
	ttlSeconds := int(maxWindow.Seconds())
	if ttlSeconds <= 0 {
		ttlSeconds = 24 * 60 * 60
	}

	if len(conds) == 0 {
		if _, err := storeEventScript.Run(ctx, s.client, []string{storeKey},
			score, member, cutoff, ttlSeconds,
		).Int(); err != nil {
			return nil, fmt.Errorf("behavior event store: store event: %w", err)
		}
		return map[string]float64{}, nil
	}

	// Group conditions by (behavior, since) to reuse ZRANGEBYSCORE results.
	type fetchKey struct {
		behavior string
		sinceMs  int64
	}
	groups := make(map[fetchKey][]model.AggregateCond)
	for _, c := range conds {
		fk := fetchKey{behavior: string(c.Behavior), sinceMs: c.Since.UnixMilli()}
		groups[fk] = append(groups[fk], c)
	}

	type pendingResult struct {
		fk  fetchKey
		cmd *goredis.StringSliceCmd
	}
	pipe := s.client.Pipeline()
	storeEventScript.Run(ctx, pipe, []string{storeKey},
		score, member, cutoff, ttlSeconds,
	)

	pending := make([]pendingResult, 0, len(groups))
	for fk := range groups {
		key := s.eventKey(event.MemberID, fk.behavior)
		cmd := pipe.ZRangeByScore(ctx, key, &goredis.ZRangeBy{
			Min: strconv.FormatInt(fk.sinceMs, 10),
			Max: "+inf",
		})
		pending = append(pending, pendingResult{fk: fk, cmd: cmd})
	}
	if _, err := pipe.Exec(ctx); err != nil && err != goredis.Nil {
		return nil, fmt.Errorf("behavior event store: pipeline exec: %w", err)
	}

	results := make(map[string]float64, len(conds))
	for _, pr := range pending {
		members, err := pr.cmd.Result()
		if err != nil && err != goredis.Nil {
			return nil, fmt.Errorf("behavior event store: zrangebyscore: %w", err)
		}
		condList := groups[pr.fk]
		schema := schemas[model.BehaviorType(pr.fk.behavior)]
		computeGroupAggregates(members, condList, schema, results)
	}
	return results, nil
}

// BatchAggregate computes multiple aggregations from Redis sorted sets.
// It groups conditions by (behavior, since) to minimize ZRANGEBYSCORE calls.
func (s *BehaviorEventStore) BatchAggregate(ctx context.Context, memberID string, conds []model.AggregateCond, schemas map[model.BehaviorType]*model.FieldSchema) (map[string]float64, error) {
	if len(conds) == 0 {
		return map[string]float64{}, nil
	}

	type fetchKey struct {
		behavior string
		sinceMs  int64
	}
	groups := make(map[fetchKey][]model.AggregateCond)
	for _, c := range conds {
		fk := fetchKey{behavior: string(c.Behavior), sinceMs: c.Since.UnixMilli()}
		groups[fk] = append(groups[fk], c)
	}

	type pendingResult struct {
		fk  fetchKey
		cmd *goredis.StringSliceCmd
	}
	pipe := s.client.Pipeline()
	pending := make([]pendingResult, 0, len(groups))
	for fk := range groups {
		key := s.eventKey(memberID, fk.behavior)
		cmd := pipe.ZRangeByScore(ctx, key, &goredis.ZRangeBy{
			Min: strconv.FormatInt(fk.sinceMs, 10),
			Max: "+inf",
		})
		pending = append(pending, pendingResult{fk: fk, cmd: cmd})
	}
	if _, err := pipe.Exec(ctx); err != nil && err != goredis.Nil {
		return nil, fmt.Errorf("behavior event store: pipeline exec: %w", err)
	}

	results := make(map[string]float64, len(conds))
	for _, pr := range pending {
		members, err := pr.cmd.Result()
		if err != nil && err != goredis.Nil {
			return nil, fmt.Errorf("behavior event store: zrangebyscore: %w", err)
		}
		condList := groups[pr.fk]
		schema := schemas[model.BehaviorType(pr.fk.behavior)]
		computeGroupAggregates(members, condList, schema, results)
	}
	return results, nil
}

// computeGroupAggregates walks a slice of encoded members once per condition,
// parsing the needed field in place. Zero allocations per entry.
func computeGroupAggregates(members []string, conds []model.AggregateCond, schema *model.FieldSchema, out map[string]float64) {
	for _, c := range conds {
		op := strings.ToUpper(c.Aggregation)
		if op == "COUNT" {
			out[c.Key] = float64(len(members))
			continue
		}
		pos := schema.Position(c.FieldPath)
		if pos < 0 {
			out[c.Key] = 0
			continue
		}
		out[c.Key] = aggregateField(op, pos, members)
	}
}

func aggregateField(op string, pos int, members []string) float64 {
	switch op {
	case "SUM":
		var sum float64
		for _, m := range members {
			if v, ok := parseFieldAt(m, pos); ok {
				sum += v
			}
		}
		return sum
	case "AVG":
		var sum float64
		var n int
		for _, m := range members {
			if v, ok := parseFieldAt(m, pos); ok {
				sum += v
				n++
			}
		}
		if n == 0 {
			return 0
		}
		return sum / float64(n)
	case "MAX":
		result := math.Inf(-1)
		found := false
		for _, m := range members {
			if v, ok := parseFieldAt(m, pos); ok {
				found = true
				if v > result {
					result = v
				}
			}
		}
		if !found {
			return 0
		}
		return result
	case "MIN":
		result := math.Inf(1)
		found := false
		for _, m := range members {
			if v, ok := parseFieldAt(m, pos); ok {
				found = true
				if v < result {
					result = v
				}
			}
		}
		if !found {
			return 0
		}
		return result
	default:
		return 0
	}
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func (s *BehaviorEventStore) eventKey(memberID, behavior string) string {
	return redisEventsPrefix + memberID + ":" + behavior
}
