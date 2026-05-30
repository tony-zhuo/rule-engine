package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaConfig configures the Kafka backend (franz-go, pure Go — see Task J
// decision notes in nats-vs-kafka.md §5).
type KafkaConfig struct {
	Topic            string        // e.g. "rule-events"
	Partition        int32         // this shard's partition (1 shard ↔ 1 partition for now)
	MaxPollRecords   int           // backpressure: cap on records per PollFetches
	SnapshotPath     string        // file to snapshot to ("" disables snapshots)
	SnapshotInterval time.Duration // how often to snapshot inline in the main loop
}

// KafkaConsumer is the franz-go pull-consumer backend for the engine. Like the
// NATS backend, it owns the source offset via the snapshot's LastSeq instead of
// the Kafka consumer-group cursor — state and source position must stay
// consistent across crashes (plan §Source 側).
//
// The caller is responsible for creating the *kgo.Client. Typical setup:
//
//	client, _ := kgo.NewClient(
//	    kgo.SeedBrokers(brokers...),
//	    kgo.FetchMaxBytes(...), kgo.FetchMaxWait(...),
//	    // NO ConsumerGroup, NO ConsumePartitions yet — Run() pins the
//	    // partition + start offset dynamically once the snapshot is restored.
//	)
type KafkaConsumer struct {
	core   *Core
	client *kgo.Client
	cfg    KafkaConfig
}

// Compile-time guarantee that KafkaConsumer satisfies EventConsumer. NATSConsumer
// has the equivalent line — having both proves the abstraction is real, not just
// rhetoric (MQ-pluggable evidence demonstrated structurally).
var _ EventConsumer = (*KafkaConsumer)(nil)

// NewKafkaConsumer wires a Core to a franz-go client with this shard's config.
func NewKafkaConsumer(core *Core, client *kgo.Client, cfg KafkaConfig) *KafkaConsumer {
	return &KafkaConsumer{core: core, client: client, cfg: cfg}
}

// Run drives the Core from Kafka. Mirrors NATSConsumer.Run step for step so the
// two backends produce identical state for the same event stream (Task L will
// shadow-compare them).
func (c *KafkaConsumer) Run(ctx context.Context) error {
	core, cfg := c.core, c.cfg

	// 1. Restore from snapshot if any → resume from LastSeq+1.
	//    Snapshot's LastSeq holds the last *offset* processed (inclusive); next
	//    record to consume is at LastSeq+1.
	var startOffset int64
	if cfg.SnapshotPath != "" {
		if data, rerr := os.ReadFile(cfg.SnapshotPath); rerr == nil {
			seq, derr := core.Restore(data)
			if derr != nil {
				return fmt.Errorf("kafka: restore snapshot: %w", derr)
			}
			startOffset = int64(seq) + 1
			slog.Info("restored from snapshot",
				"shard", core.ShardID, "backend", "kafka", "last_offset", seq)
		}
	}

	// 2. Determine the replay target = the end-of-log at startup. Anything from
	//    startOffset up to and including endOff.Offset-1 is replay; once we cross
	//    that, we're caught up to live and side effects are re-enabled.
	adm := kadm.NewClient(c.client)
	listed, err := adm.ListEndOffsets(ctx, cfg.Topic)
	if err != nil {
		return fmt.Errorf("kafka: list end offsets: %w", err)
	}
	endOff := listed[cfg.Topic][cfg.Partition]
	if endOff.Err != nil {
		return fmt.Errorf("kafka: end offset for %s/%d: %w",
			cfg.Topic, cfg.Partition, endOff.Err)
	}
	targetOffset := endOff.Offset - 1 // last record's offset; -1 when log is empty
	replaying := targetOffset >= 0 && startOffset <= targetOffset
	if replaying {
		core.BeginReplay()
		slog.Info("replaying",
			"shard", core.ShardID, "backend", "kafka",
			"from", startOffset, "to", targetOffset)
	}

	// 3. Pin partition + start offset — we own the position, not the consumer
	//    group cursor. AddConsumePartitions takes effect for subsequent polls.
	c.client.AddConsumePartitions(map[string]map[int32]kgo.Offset{
		cfg.Topic: {cfg.Partition: kgo.NewOffset().At(startOffset)},
	})

	// 4. Main loop: PollFetches → decode → ProcessEvent → update lastSeq.
	//    Single goroutine = single writer (the property keeping state lock-free).
	lastSnapshot := time.Now()
	for {
		fetches := c.client.PollFetches(ctx)

		// Clean shutdown on ctx cancel — PollFetches returns immediately when
		// ctx is done; we detect via ctx.Err() rather than scanning errors.
		if ctx.Err() != nil {
			if cfg.SnapshotPath != "" {
				if serr := snapshotToFile(core, cfg.SnapshotPath); serr != nil {
					slog.Error("final snapshot failed",
						"shard", core.ShardID, "backend", "kafka", "error", serr)
				}
			}
			return nil
		}

		// Other fetch errors (broker hiccup, partition leader change) get logged
		// but don't kill the loop — franz-go retries internally.
		if errs := fetches.Errors(); len(errs) > 0 {
			for _, e := range errs {
				slog.Warn("kafka fetch error",
					"shard", core.ShardID,
					"topic", e.Topic, "partition", e.Partition, "error", e.Err)
			}
		}

		fetches.EachRecord(func(r *kgo.Record) {
			event, derr := decodeEvent(r.Value)
			if derr != nil {
				slog.Warn("dropping malformed event",
					"shard", core.ShardID, "backend", "kafka",
					"offset", r.Offset, "error", derr)
				return
			}
			core.ProcessEvent(event)
			core.lastSeq.Store(uint64(r.Offset))
			if replaying && r.Offset >= targetOffset {
				core.EndReplay()
				replaying = false
				slog.Info("caught up to live",
					"shard", core.ShardID, "backend", "kafka", "offset", r.Offset)
			}
		})

		// 5. Backpressure is the polling rate itself — we process before the
		//    next Poll, so a slow Core naturally throttles fetching. FetchMaxBytes
		//    on the client caps per-poll volume if explicit ceiling is needed.

		// 6. Inline snapshot (single-writer; gap #20 — async would need COW).
		if cfg.SnapshotPath != "" && cfg.SnapshotInterval > 0 &&
			time.Since(lastSnapshot) >= cfg.SnapshotInterval {
			if serr := snapshotToFile(core, cfg.SnapshotPath); serr != nil {
				slog.Error("snapshot failed",
					"shard", core.ShardID, "backend", "kafka", "error", serr)
			}
			lastSnapshot = time.Now()
		}
	}
}
