package usecase

import (
	"strings"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// CollectUniqueAggregateKeys extracts deduplicated aggregate keys from compiled strategies.
func CollectUniqueAggregateKeys(strategies []model.CompiledStrategy) []model.AggregateKey {
	seen := make(map[string]struct{})
	var keys []model.AggregateKey
	for _, cs := range strategies {
		for _, k := range cs.AggregateKeys {
			ck := k.CacheKey()
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// BuildBehaviorSchemas derives per-behavior field schemas from compiled strategies.
// Each schema lists the numeric fields (in deterministic order) referenced by
// aggregation rules for a given behavior. This drives the zero-alloc pipe-separated
// event encoding in the Redis behavior store.
//
// Example: rules "CryptoWithdraw:SUM:amount" and "CryptoWithdraw:AVG:fee"
// → CryptoWithdraw schema has Fields=["amount", "fee"]
// COUNT-only rules (no field path) do not populate any schema.
func BuildBehaviorSchemas(strategies []model.CompiledStrategy) map[behaviorModel.BehaviorType]*behaviorModel.FieldSchema {
	// Collect unique fields per behavior in deterministic order (insertion order).
	type perBehavior struct {
		fields []string
		seen   map[string]struct{}
	}
	byBehavior := make(map[behaviorModel.BehaviorType]*perBehavior)

	for _, cs := range strategies {
		for _, k := range cs.AggregateKeys {
			parts := strings.SplitN(k.Field, ":", 3)
			if len(parts) < 3 || parts[2] == "" {
				continue // COUNT-style: no field path
			}
			behavior := behaviorModel.BehaviorType(parts[0])
			field := parts[2]

			pb, ok := byBehavior[behavior]
			if !ok {
				pb = &perBehavior{seen: make(map[string]struct{})}
				byBehavior[behavior] = pb
			}
			if _, dup := pb.seen[field]; dup {
				continue
			}
			pb.seen[field] = struct{}{}
			pb.fields = append(pb.fields, field)
		}
	}

	out := make(map[behaviorModel.BehaviorType]*behaviorModel.FieldSchema, len(byBehavior))
	for behavior, pb := range byBehavior {
		index := make(map[string]int, len(pb.fields))
		for i, f := range pb.fields {
			index[f] = i
		}
		out[behavior] = &behaviorModel.FieldSchema{
			Fields: pb.fields,
			Index:  index,
		}
	}
	return out
}

// MaxWindowFromKeys returns the longest time window across all aggregate keys.
// This determines how long events need to be retained in Redis.
func MaxWindowFromKeys(keys []model.AggregateKey) time.Duration {
	var max time.Duration
	for _, k := range keys {
		if d := k.Window.Duration(); d > max {
			max = d
		}
	}
	return max
}

// BuildAggregateConds converts rule aggregate keys into behavior aggregate conditions.
func BuildAggregateConds(memberID string, keys []model.AggregateKey, now time.Time) []behaviorModel.AggregateCond {
	conds := make([]behaviorModel.AggregateCond, 0, len(keys))
	for _, k := range keys {
		conds = append(conds, *buildAggregateCond(memberID, k, now))
	}
	return conds
}

func buildAggregateCond(memberID string, k model.AggregateKey, now time.Time) *behaviorModel.AggregateCond {
	parts := strings.SplitN(k.Field, ":", 3)
	fieldPath := ""
	if len(parts) == 3 {
		fieldPath = parts[2]
	}

	var since time.Time
	if k.Window != nil {
		switch strings.ToLower(k.Window.Unit) {
		case "minutes":
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		case "hours":
			since = now.Add(-time.Duration(k.Window.Value) * time.Hour)
		case "days":
			since = now.Add(-time.Duration(k.Window.Value) * 24 * time.Hour)
		default:
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		}
	}

	return &behaviorModel.AggregateCond{
		MemberID:    memberID,
		Behavior:    behaviorModel.BehaviorType(parts[0]),
		Aggregation: strings.ToUpper(parts[1]),
		FieldPath:   fieldPath,
		Since:       since,
		Key:         k.CacheKey(),
	}
}
