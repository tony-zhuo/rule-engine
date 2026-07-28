package usecase

import (
	"time"

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
// This bounds how long the engine retains time buckets before pruning them.
func MaxWindowFromKeys(keys []model.AggregateKey) time.Duration {
	var max time.Duration
	for _, k := range keys {
		if d := k.Window.Duration(); d > max {
			max = d
		}
	}
	return max
}
