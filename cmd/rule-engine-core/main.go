// Command rule-engine-core runs one shard of the in-memory, event-sourced rule
// engine: it consumes behavioral events from NATS JetStream or Kafka (BACKEND
// env, default nats), evaluates rules and CEP patterns entirely in memory, and
// snapshots periodically for fast recovery.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/pkg/env"
	cepDB "github.com/tony-zhuo/rule-engine/service/base/cep/repository/db"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	"github.com/tony-zhuo/rule-engine/service/engine/core"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	// Control plane lives in PostgreSQL only — rule_strategies + cep_patterns,
	// read once at startup. The shard needs no other external store: its event
	// state is in memory and the rule cache is an in-process atomic.Pointer.
	pkgdb.Init(cfg.DB)
	db := pkgdb.GetDB()

	strategyUC := ruleUsecase.NewRuleStrategyUsecase(
		ruleDB.NewRuleStrategyRepo(db), ruleUsecase.NewRuleUsecase(),
	)
	ruleSet, err := strategyUC.ListActiveCompiled(ctx)
	if err != nil {
		log.Fatal("load compiled rules: ", err)
	}

	// Load CEP patterns.
	patterns, err := cepDB.NewCEPPatternRepo(db).ListActive(ctx)
	if err != nil {
		log.Fatal("load cep patterns: ", err)
	}

	// Engine-specific settings (per-shard) come from the environment.
	backend := env.Str("BACKEND", "nats")
	shardID := env.Int("SHARD_ID", 0)
	snapshotDir := env.Str("SNAPSHOT_DIR", "")
	snapshotPath := ""
	if snapshotDir != "" {
		snapshotPath = fmt.Sprintf("%s/shard_%d.snap", snapshotDir, shardID)
	}

	// Build this shard's engine and register its CEP patterns.
	engine := core.NewCore(shardID, ruleSet)
	for _, p := range patterns {
		if err := engine.AddPattern(p); err != nil {
			log.Fatal("add pattern: ", err)
		}
	}

	var consumer core.EventConsumer
	var shutdown func()
	switch backend {
	case "nats":
		consumer, shutdown = setupNATSConsumer(engine, shardID, snapshotPath)
	case "kafka":
		consumer, shutdown = setupKafkaConsumer(engine, shardID, snapshotPath)
	default:
		log.Fatalf("unknown BACKEND=%q (want: nats|kafka)", backend)
	}
	defer shutdown()

	slog.Info("rule-engine-core starting",
		"shard", shardID, "backend", backend,
		"rules", len(ruleSet.Strategies), "patterns", len(patterns))

	if err := consumer.Run(ctx); err != nil {
		log.Fatal("engine run: ", err)
	}
	slog.Info("rule-engine-core stopped", "shard", shardID)
}

// setupNATSConsumer connects to NATS JetStream and returns this shard's
// consumer + shutdown closure for the connection.
func setupNATSConsumer(engine *core.Core, shardID int, snapshotPath string) (core.EventConsumer, func()) {
	natsURL := env.Str("NATS_URL", nats.DefaultURL)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatal("connect nats: ", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		log.Fatal("jetstream: ", err)
	}

	return core.NewNATSConsumer(engine, js, core.NATSConfig{
			StreamName:       "rule-events",
			Subjects:         []string{"rule.events.>"},
			FilterSubject:    fmt.Sprintf("rule.events.%d.>", shardID),
			MaxAckPending:    1000,
			SnapshotPath:     snapshotPath,
			SnapshotInterval: 60 * time.Second,
		}),
		func() { nc.Close() }
}

// setupKafkaConsumer builds a franz-go client for this shard and returns the
// consumer + shutdown closure. No ConsumerGroup and no ConsumePartitions here:
// KafkaConsumer.Run pins the partition + start offset itself once the snapshot
// is restored (the engine owns the source offset, not the broker cursor).
func setupKafkaConsumer(engine *core.Core, shardID int, snapshotPath string) (core.EventConsumer, func()) {
	brokers := strings.Split(env.Str("KAFKA_BROKERS", "localhost:9092"), ",")
	topic := env.Str("TOPIC", "rule-events")

	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		log.Fatal("kafka client: ", err)
	}

	return core.NewKafkaConsumer(engine, client, core.KafkaConfig{
			Topic:            topic,
			Partition:        int32(shardID),
			MaxPollRecords:   1000,
			SnapshotPath:     snapshotPath,
			SnapshotInterval: 60 * time.Second,
		}),
		func() { client.Close() }
}
