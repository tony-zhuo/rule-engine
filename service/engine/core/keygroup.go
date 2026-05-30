// Package core implements the in-memory, event-sourced rule engine.
//
// A member's events are processed by exactly one shard, and within a shard by
// a single goroutine — this single-writer model is what lets aggregation and
// CEP state be updated without locks (see Core.ProcessEvent).
package core

import "hash/crc32"

// NumKeyGroups is the fixed number of key groups. This is a day-1 commitment:
// changing it would re-map every member to a different key group, defeating the
// whole point of the abstraction. 128 mirrors Flink's default maxParallelism —
// large enough that any realistic shard count gets a handful of key groups each,
// small enough that per-key-group snapshot files stay coarse.
const NumKeyGroups = 128

// KeyGroupID identifies one of the NumKeyGroups logical partitions.
// It is an internal concept: NATS never sees it (see plan §1).
type KeyGroupID int

// KeyGroupOf maps a member to its key group. This mapping never changes for the
// lifetime of the system, so a member's snapshot data always lives under the
// same key group directory regardless of how shards are later added or removed.
func KeyGroupOf(memberID string) KeyGroupID {
	return KeyGroupID(crc32.ChecksumIEEE([]byte(memberID)) % NumKeyGroups)
}

// ShardOf maps a key group to a shard for a given shard count. Unlike KeyGroupOf,
// this mapping is a runtime configuration: rescaling from N to M shards only
// recomputes these NumKeyGroups assignments, never re-hashes members.
func ShardOf(kg KeyGroupID, numShards int) int {
	return int(kg) * numShards / NumKeyGroups
}

// ShardOfMember is the convenience composition used by producers to decide which
// shard subject to publish to.
func ShardOfMember(memberID string, numShards int) int {
	return ShardOf(KeyGroupOf(memberID), numShards)
}
