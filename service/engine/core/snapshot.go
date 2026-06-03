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
type snapshot struct {
	State          *ShardState
	WatermarkNanos int64
	LastSeq        uint64
}

// Snapshot serializes the shard's state with gob. gob encodes only exported
// fields, which is why the whole state tree was made exported back in Task A.
//
// This is the slice's simple whole-shard snapshot; the plan's per-key-group +
// manifest incremental format (plan §Snapshot 格式) is a later refinement.
func (c *Core) Snapshot() ([]byte, error) {
	snap := snapshot{
		State:          c.State,
		WatermarkNanos: c.watermark.Load(),
		LastSeq:        c.lastSeq.Load(),
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&snap); err != nil {
		return nil, fmt.Errorf("snapshot encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Restore replaces the shard's state from a snapshot, returning the NATS sequence
// the state reflects. The caller resumes consuming from LastSeq+1, replaying the
// events newer than the snapshot — idempotent on event_id, so any overlap is safe.
func (c *Core) Restore(data []byte) (lastSeq uint64, err error) {
	var snap snapshot
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&snap); err != nil {
		return 0, fmt.Errorf("snapshot decode: %w", err)
	}
	c.State = snap.State
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
	for _, ms := range c.State.Members {
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
	}
	heap.Init(&c.negDeadlines)
}
