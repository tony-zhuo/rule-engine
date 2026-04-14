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
