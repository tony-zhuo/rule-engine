package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// NATSConfig configures the NATS JetStream backend.
type NATSConfig struct {
	StreamName       string        // JetStream stream holding the event log
	Subjects         []string      // stream subjects (e.g. ["rule.events.>"])
	FilterSubject    string        // this shard's filter (e.g. "rule.events.0.>")
	MaxAckPending    int           // backpressure: max unacked messages in flight
	SnapshotPath     string        // file to snapshot to ("" disables snapshots)
	SnapshotInterval time.Duration // how often to snapshot inline in the main loop
}

// NATSConsumer is the JetStream pull-consumer backend for the engine. It owns
// the source offset via the snapshot's LastSeq (not NATS' durable cursor), so
// state and source position stay consistent across crashes (plan §Source 側).
type NATSConsumer struct {
	core *Core
	js   jetstream.JetStream
	cfg  NATSConfig
}

// Compile-time guarantee that NATSConsumer satisfies the EventConsumer
// interface — when Task J adds KafkaConsumer this same line will exist for it,
// proving the abstraction is real.
var _ EventConsumer = (*NATSConsumer)(nil)

// NewNATSConsumer wires a Core to a JetStream context with this shard's config.
func NewNATSConsumer(core *Core, js jetstream.JetStream, cfg NATSConfig) *NATSConsumer {
	return &NATSConsumer{core: core, js: js, cfg: cfg}
}

// Run drives the Core from JetStream. Single goroutine = single writer, which
// is what keeps state in core/state.go lock-free.
func (c *NATSConsumer) Run(ctx context.Context) error {
	core, cfg := c.core, c.cfg

	stream, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     cfg.StreamName,
		Subjects: cfg.Subjects,
	})
	if err != nil {
		return fmt.Errorf("nats: ensure stream: %w", err)
	}

	// 1. Restore from snapshot if one exists; resume from the next sequence.
	startSeq := uint64(1)
	if cfg.SnapshotPath != "" {
		if data, rerr := os.ReadFile(cfg.SnapshotPath); rerr == nil {
			seq, derr := core.Restore(data)
			if derr != nil {
				return fmt.Errorf("nats: restore snapshot: %w", derr)
			}
			startSeq = seq + 1
			slog.Info("restored from snapshot", "shard", core.ShardID, "last_seq", seq)
		}
	}

	// 2. Anything between startSeq and the current live edge is replay.
	info, err := stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("nats: stream info: %w", err)
	}
	targetSeq := info.State.LastSeq
	replaying := targetSeq > 0 && startSeq <= targetSeq
	if replaying {
		core.BeginReplay()
		slog.Info("replaying", "shard", core.ShardID, "from", startSeq, "to", targetSeq)
	}

	// 3. Ephemeral pull consumer starting at our checkpointed sequence — we own
	//    the offset, not NATS' durable cursor (plan §Source 側).
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		AckPolicy:      jetstream.AckExplicitPolicy,
		MaxAckPending:  cfg.MaxAckPending,
		FilterSubjects: []string{cfg.FilterSubject},
		DeliverPolicy:  jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:    startSeq,
	})
	if err != nil {
		return fmt.Errorf("nats: create consumer: %w", err)
	}

	iter, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("nats: messages iterator: %w", err)
	}
	defer iter.Stop()
	go func() { <-ctx.Done(); iter.Stop() }()

	// 4. Main loop: pull → process → ack. Single goroutine = single writer.
	lastSnapshot := time.Now()
	for {
		msg, err := iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) || ctx.Err() != nil {
				// Final snapshot on clean shutdown so the next start has a fresh
				// checkpoint to resume from.
				if cfg.SnapshotPath != "" {
					if serr := snapshotToFile(core, cfg.SnapshotPath); serr != nil {
						slog.Error("final snapshot failed", "shard", core.ShardID, "error", serr)
					}
				}
				return nil
			}
			return fmt.Errorf("nats: next message: %w", err)
		}

		event, derr := decodeEvent(msg.Data())
		if derr != nil {
			_ = msg.Term()
			slog.Warn("dropping malformed event", "shard", core.ShardID, "error", derr)
			continue
		}

		core.ProcessEvent(event)

		if err := msg.Ack(); err != nil {
			return fmt.Errorf("nats: ack: %w", err)
		}
		if meta, merr := msg.Metadata(); merr == nil {
			seq := meta.Sequence.Stream
			core.lastSeq.Store(seq)
			if replaying && seq >= targetSeq {
				core.EndReplay()
				replaying = false
				slog.Info("caught up to live", "shard", core.ShardID, "seq", seq)
			}
		}

		// 5. Inline snapshot (single-writer; gap #20 — async would need COW).
		if cfg.SnapshotPath != "" && cfg.SnapshotInterval > 0 && time.Since(lastSnapshot) >= cfg.SnapshotInterval {
			if serr := snapshotToFile(core, cfg.SnapshotPath); serr != nil {
				slog.Error("snapshot failed", "shard", core.ShardID, "error", serr)
			}
			lastSnapshot = time.Now()
		}
	}
}
