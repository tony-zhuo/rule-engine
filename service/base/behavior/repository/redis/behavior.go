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

const redisEventsPrefix = "rule_engine:events:"

// BehaviorEventStore stores behavioral events in Redis Sorted Sets for
// real-time aggregation queries, replacing PostgreSQL on the hot path.
//
// Key schema: rule_engine:events:{member_id}:{behavior}
// Score: occurred_at as Unix milliseconds
// Member: {event_id}\x00{json_fields}
type BehaviorEventStore struct {
	client *goredis.Client
}

func NewBehaviorEventStore(client *goredis.Client) *BehaviorEventStore {
	return &BehaviorEventStore{client: client}
}

// storeEventScript atomically: ZADD NX + ZREMRANGEBYSCORE (prune) + EXPIRE.
// KEYS[1] = sorted set key
// ARGV[1] = score (occurred_at millis)
// ARGV[2] = member value (event_id \x00 json_fields)
// ARGV[3] = cutoff score for pruning
// ARGV[4] = TTL in seconds
// Returns 1 if newly added, 0 if duplicate.
var storeEventScript = goredis.NewScript(`
local added = redis.call('ZADD', KEYS[1], 'NX', ARGV[1], ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', ARGV[3])
redis.call('EXPIRE', KEYS[1], ARGV[4])
return added
`)

func (s *BehaviorEventStore) StoreEvent(ctx context.Context, event *model.BehaviorEvent, maxWindow time.Duration) (bool, error) {
	key := s.eventKey(event.MemberID, string(event.Behavior))
	score := float64(event.OccurredAt.UnixMilli())

	fieldsJSON, err := json.Marshal(event.Fields)
	if err != nil {
		return false, fmt.Errorf("behavior event store: marshal fields: %w", err)
	}
	member := event.EventID + "\x00" + string(fieldsJSON)

	cutoff := time.Now().Add(-maxWindow).UnixMilli()
	ttlSeconds := int(maxWindow.Seconds())
	if ttlSeconds <= 0 {
		ttlSeconds = 24 * 60 * 60 // safety floor: 1 day
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
func (s *BehaviorEventStore) StoreAndAggregate(ctx context.Context, event *model.BehaviorEvent, conds []model.AggregateCond, maxWindow time.Duration) (map[string]float64, error) {
	// Prepare store event args.
	storeKey := s.eventKey(event.MemberID, string(event.Behavior))
	score := float64(event.OccurredAt.UnixMilli())

	fieldsJSON, err := json.Marshal(event.Fields)
	if err != nil {
		return nil, fmt.Errorf("behavior event store: marshal fields: %w", err)
	}
	member := event.EventID + "\x00" + string(fieldsJSON)
	cutoff := time.Now().Add(-maxWindow).UnixMilli()
	ttlSeconds := int(maxWindow.Seconds())
	if ttlSeconds <= 0 {
		ttlSeconds = 24 * 60 * 60
	}

	if len(conds) == 0 {
		// No aggregations needed — just store.
		_, err := storeEventScript.Run(ctx, s.client, []string{storeKey},
			score, member, cutoff, ttlSeconds,
		).Int()
		if err != nil {
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

	// Single pipeline: store Lua + all ZRANGEBYSCORE commands.
	type pendingResult struct {
		fk  fetchKey
		cmd *goredis.StringSliceCmd
	}
	pipe := s.client.Pipeline()

	// 1. Store event (Lua script via pipeline).
	storeEventScript.Run(ctx, pipe, []string{storeKey},
		score, member, cutoff, ttlSeconds,
	)

	// 2. All ZRANGEBYSCORE commands in the same pipeline.
	var pending []pendingResult
	for fk := range groups {
		key := s.eventKey(event.MemberID, fk.behavior)
		cmd := pipe.ZRangeByScore(ctx, key, &goredis.ZRangeBy{
			Min: strconv.FormatInt(fk.sinceMs, 10),
			Max: "+inf",
		})
		pending = append(pending, pendingResult{fk: fk, cmd: cmd})
	}

	// One round-trip for everything.
	if _, err := pipe.Exec(ctx); err != nil && err != goredis.Nil {
		return nil, fmt.Errorf("behavior event store: pipeline exec: %w", err)
	}

	// Parse and compute aggregations in Go (no Redis time).
	entriesByGroup := make(map[fetchKey][]eventEntry)
	for _, pr := range pending {
		members, err := pr.cmd.Result()
		if err != nil && err != goredis.Nil {
			return nil, fmt.Errorf("behavior event store: zrangebyscore: %w", err)
		}
		entries := make([]eventEntry, 0, len(members))
		for _, m := range members {
			idx := strings.IndexByte(m, '\x00')
			if idx < 0 {
				continue
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(m[idx+1:]), &fields); err != nil {
				continue
			}
			entries = append(entries, eventEntry{fields: fields})
		}
		entriesByGroup[pr.fk] = entries
	}

	results := make(map[string]float64, len(conds))
	for fk, condList := range groups {
		entries := entriesByGroup[fk]
		for _, c := range condList {
			results[c.Key] = computeAggregate(c.Aggregation, c.FieldPath, entries)
		}
	}
	return results, nil
}

// BatchAggregate computes multiple aggregations from Redis sorted sets.
// It groups conditions by (behavior, since) to minimize ZRANGEBYSCORE calls,
// then computes aggregations in Go from the fetched entries.
func (s *BehaviorEventStore) BatchAggregate(ctx context.Context, memberID string, conds []model.AggregateCond) (map[string]float64, error) {
	if len(conds) == 0 {
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

	// Pipeline: one ZRANGEBYSCORE per unique (behavior, since).
	type pendingResult struct {
		fk  fetchKey
		cmd *goredis.StringSliceCmd
	}
	pipe := s.client.Pipeline()
	var pending []pendingResult
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

	// Parse fetched entries into field maps per group.
	entriesByGroup := make(map[fetchKey][]eventEntry)
	for _, pr := range pending {
		members, err := pr.cmd.Result()
		if err != nil && err != goredis.Nil {
			return nil, fmt.Errorf("behavior event store: zrangebyscore: %w", err)
		}
		entries := make([]eventEntry, 0, len(members))
		for _, m := range members {
			idx := strings.IndexByte(m, '\x00')
			if idx < 0 {
				continue // malformed entry
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(m[idx+1:]), &fields); err != nil {
				continue // skip unparseable
			}
			entries = append(entries, eventEntry{fields: fields})
		}
		entriesByGroup[pr.fk] = entries
	}

	// Compute aggregations.
	results := make(map[string]float64, len(conds))
	for fk, condList := range groups {
		entries := entriesByGroup[fk]
		for _, c := range condList {
			results[c.Key] = computeAggregate(c.Aggregation, c.FieldPath, entries)
		}
	}
	return results, nil
}

type eventEntry struct {
	fields map[string]any
}

func computeAggregate(agg string, fieldPath string, entries []eventEntry) float64 {
	switch strings.ToUpper(agg) {
	case "COUNT":
		return float64(len(entries))
	case "SUM":
		return sumField(fieldPath, entries)
	case "AVG":
		if len(entries) == 0 {
			return 0
		}
		return sumField(fieldPath, entries) / float64(len(entries))
	case "MAX":
		return maxField(fieldPath, entries)
	case "MIN":
		return minField(fieldPath, entries)
	default:
		return 0
	}
}

func sumField(fieldPath string, entries []eventEntry) float64 {
	var total float64
	for _, e := range entries {
		if v, ok := extractFloat(e.fields, fieldPath); ok {
			total += v
		}
	}
	return total
}

func maxField(fieldPath string, entries []eventEntry) float64 {
	result := math.Inf(-1)
	found := false
	for _, e := range entries {
		if v, ok := extractFloat(e.fields, fieldPath); ok {
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
}

func minField(fieldPath string, entries []eventEntry) float64 {
	result := math.Inf(1)
	found := false
	for _, e := range entries {
		if v, ok := extractFloat(e.fields, fieldPath); ok {
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
}

func extractFloat(fields map[string]any, path string) (float64, bool) {
	v, ok := fields[path]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
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
