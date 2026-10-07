package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// EventConsumer drives the Core from an event source (NATS, Kafka, ...). Each
// backend wraps its own connection + config and implements Run; the Core itself
// stays MQ-agnostic — files A–G in this package never import any MQ library.
//
// The lifecycle is the same regardless of backend:
//   1. Load snapshot from disk (if any) → resume from LastSeq+1.
//   2. Anything between LastSeq+1 and the live edge is replay (side effects
//      suppressed via core.BeginReplay).
//   3. Pull loop: decode → core.ProcessEvent → ack → update lastSeq. Periodic
//      snapshots freeze the state on the main goroutine and encode + write it
//      in the background; copy-on-write (state.go) keeps the frozen view
//      consistent while the loop keeps writing (gap #20).
//   4. Final snapshot on clean shutdown, after any in-flight one finishes.
type EventConsumer interface {
	Run(ctx context.Context) error
}

// decodeEvent parses a JSON-encoded event from an MQ message body. Both backends
// agree on JSON-on-the-wire for now; a future Schema-Registry-backed protobuf
// path would be a per-backend concern.
func decodeEvent(data []byte) (*behaviorModel.BehaviorEvent, error) {
	var ev behaviorModel.BehaviorEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	if ev.EventID == "" || ev.MemberID == "" {
		return nil, fmt.Errorf("decode event: missing event_id or member_id")
	}
	return &ev, nil
}

// snapshotToFile writes a synchronous snapshot of the shard. Shared by both
// backends since the snapshot itself is MQ-agnostic.
func snapshotToFile(core *Core, path string) error {
	data, err := core.Snapshot()
	if err != nil {
		return err
	}
	return writeSnapshotFile(path, data)
}

// writeSnapshotFile writes atomically (temp + rename), so a crash mid-write
// never leaves a half-written snapshot in place.
func writeSnapshotFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// asyncSnapshotter runs the consumers' periodic snapshots: beginSnapshot on the
// main goroutine (µs), encode + write in a background goroutine. At most one is
// in flight; an interval that elapses while one is still running is skipped.
// All methods are called from the main goroutine only.
type asyncSnapshotter struct {
	path     string
	interval time.Duration
	last     time.Time
	done     chan error // non-nil while a snapshot is in flight
}

func newAsyncSnapshotter(path string, interval time.Duration) *asyncSnapshotter {
	return &asyncSnapshotter{path: path, interval: interval, last: time.Now()}
}

// tick is called after each processed event: it collects a finished background
// snapshot (releasing the frozen state) and starts a new one when due.
func (s *asyncSnapshotter) tick(core *Core) {
	if s.path == "" || s.interval <= 0 {
		return
	}
	if s.done != nil {
		select {
		case err := <-s.done:
			s.finish(core, err)
		default:
			return
		}
	}
	if time.Since(s.last) < s.interval {
		return
	}
	s.last = time.Now()
	f := core.beginSnapshot()
	done := make(chan error, 1)
	s.done = done
	go func() {
		data, err := f.encode()
		if err == nil {
			err = writeSnapshotFile(s.path, data)
		}
		done <- err
	}()
}

// drain waits for any in-flight snapshot, then writes the final snapshot on
// clean shutdown so the next start has a fresh checkpoint to resume from.
func (s *asyncSnapshotter) drain(core *Core) error {
	if s.path == "" {
		return nil
	}
	if s.done != nil {
		s.finish(core, <-s.done)
	}
	return snapshotToFile(core, s.path)
}

func (s *asyncSnapshotter) finish(core *Core, err error) {
	core.endSnapshot()
	s.done = nil
	if err != nil {
		slog.Error("snapshot failed", "shard", core.ShardID, "error", err)
	}
}
