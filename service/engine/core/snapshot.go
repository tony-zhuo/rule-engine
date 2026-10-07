package core

import (
	"bytes"
	"container/heap"
	"encoding/gob"
	"fmt"
)

func init() {
	// CEP Variables (map[string]any) and similar carry concrete values through an
	// interface; gob must know the concrete types to encode/decode them.
	gob.Register("")
	gob.Register(float64(0))
}

// snapshot is the serializable form of a shard's recoverable state. It pairs the
// in-memory state with the source position (NATS seq) it reflects, so on restart
// we replay from exactly LastSeq+1 — no gap, no double application (plan §Checkpoint).
// Members are stored per key group; key groups never written are omitted.
type snapshot struct {
	KeyGroups      map[KeyGroupID]map[string]*MemberState
	WatermarkNanos int64
	LastSeq        uint64
}

// frozenSnapshot is a point-in-time view taken by beginSnapshot. Its key group
// array is a copy, and copy-on-write keeps every map and member it reaches
// unmodified until endSnapshot, so encode may run on any goroutine.
type frozenSnapshot struct {
	state          ShardState
	watermarkNanos int64
	lastSeq        uint64
}

// beginSnapshot freezes the current state. It only copies the key group pointer
// array, so the main loop pauses for microseconds rather than for the encode.
// Main goroutine only; at most one snapshot may be in flight.
func (c *Core) beginSnapshot() *frozenSnapshot {
	return &frozenSnapshot{
		state:          c.freezeState(),
		watermarkNanos: c.watermark.Load(),
		lastSeq:        c.lastSeq.Load(),
	}
}

// endSnapshot releases the frozen view once encode has returned, so writes stop
// paying for copy-on-write clones. Main goroutine only.
func (c *Core) endSnapshot() {
	c.unfreezeState()
}

// encode serializes the frozen view with gob. Safe to call off the main goroutine.
func (f *frozenSnapshot) encode() ([]byte, error) {
	snap := snapshot{
		KeyGroups:      make(map[KeyGroupID]map[string]*MemberState),
		WatermarkNanos: f.watermarkNanos,
		LastSeq:        f.lastSeq,
	}
	for id, kg := range f.state.keyGroups {
		if kg != nil {
			snap.KeyGroups[KeyGroupID(id)] = kg.Members
		}
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&snap); err != nil {
		return nil, fmt.Errorf("snapshot encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Snapshot serializes the shard's state synchronously: freeze, encode, release.
// Used for the final snapshot on shutdown and in tests; the consumers' periodic
// snapshots run encode in the background instead (consumer.go asyncSnapshotter).
func (c *Core) Snapshot() ([]byte, error) {
	f := c.beginSnapshot()
	defer c.endSnapshot()
	return f.encode()
}

// Restore replaces the shard's state from a snapshot, returning the NATS sequence
// the state reflects. The caller resumes consuming from LastSeq+1, replaying the
// events newer than the snapshot — idempotent on event_id, so any overlap is safe.
func (c *Core) Restore(data []byte) (lastSeq uint64, err error) {
	var snap snapshot
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&snap); err != nil {
		return 0, fmt.Errorf("snapshot decode: %w", err)
	}
	state := NewShardState()
	for id, members := range snap.KeyGroups {
		if id < 0 || int(id) >= NumKeyGroups {
			return 0, fmt.Errorf("snapshot decode: key group %d out of range [0,%d)", id, NumKeyGroups)
		}
		if members != nil {
			state.keyGroups[id] = &keyGroupState{Members: members}
		}
	}
	c.State = state
	c.watermark.Store(snap.WatermarkNanos)
	c.lastSeq.Store(snap.LastSeq)
	c.rebuildNegativeDeadlines()
	return snap.LastSeq, nil
}

// rebuildNegativeDeadlines reconstructs the negative-deadline heap from the
// restored ShardState. The heap itself isn't serialized — each progress carries
// its own NegativeDeadline, so we walk the state once after restore to rebuild
// the priority structure.
func (c *Core) rebuildNegativeDeadlines() {
	c.negDeadlines = c.negDeadlines[:0]
	c.State.forEachMember(func(ms *MemberState) {
		for _, p := range ms.Progresses {
			if p.NegativeDeadline.IsZero() {
				continue
			}
			c.negDeadlines = append(c.negDeadlines, negDeadlineEntry{
				Deadline:   p.NegativeDeadline,
				MemberID:   p.MemberID,
				ProgressID: p.ID,
			})
		}
	})
	heap.Init(&c.negDeadlines)
}
