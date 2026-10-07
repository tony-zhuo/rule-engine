package core

import (
	"fmt"
	"testing"
)

func TestKeyGroupOf_InRange(t *testing.T) {
	if NumKeyGroups != 1024 {
		t.Fatalf("NumKeyGroups = %d, want 1024", NumKeyGroups)
	}
	for i := 0; i < 100_000; i++ {
		kg := KeyGroupOf(fmt.Sprintf("member-%d", i))
		if kg < 0 || int(kg) >= NumKeyGroups {
			t.Fatalf("KeyGroupOf out of range: %d", kg)
		}
	}
}

func TestShardOf_ContiguousAndBalanced(t *testing.T) {
	tests := []struct {
		name      string
		numShards int
	}{
		{"single shard", 1},
		{"non-divisor", 3},
		{"four shards", 4},
		{"sixty-four shards", 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counts := make([]int, tt.numShards)
			prev := 0
			for kg := 0; kg < NumKeyGroups; kg++ {
				shard := ShardOf(KeyGroupID(kg), tt.numShards)
				if shard < 0 || shard >= tt.numShards {
					t.Fatalf("kg %d → shard %d, out of [0,%d)", kg, shard, tt.numShards)
				}
				if shard < prev {
					t.Fatalf("kg %d → shard %d after shard %d: ranges not contiguous", kg, shard, prev)
				}
				prev = shard
				counts[shard]++
			}
			minC, maxC := counts[0], counts[0]
			for _, c := range counts {
				minC, maxC = min(minC, c), max(maxC, c)
			}
			if maxC-minC > 1 {
				t.Fatalf("unbalanced kg assignment: min %d, max %d (%v)", minC, maxC, counts)
			}
		})
	}
}
