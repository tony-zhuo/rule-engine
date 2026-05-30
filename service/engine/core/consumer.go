package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
//   3. Pull loop: decode → core.ProcessEvent → ack → update lastSeq. Inline
//      snapshot in the same goroutine (single-writer; gap #20 is why we don't
//      run snapshot in a background goroutine).
//   4. Final snapshot on clean shutdown.
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

// snapshotToFile writes the shard's snapshot atomically (temp + rename), so a
// crash mid-write never leaves a half-written snapshot in place. Shared by both
// backends since the snapshot itself is MQ-agnostic.
func snapshotToFile(core *Core, path string) error {
	data, err := core.Snapshot()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
