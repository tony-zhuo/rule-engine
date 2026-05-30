// Command rule-engine-core runs one shard of the in-memory, event-sourced rule
// engine: it consumes behavioral events from NATS JetStream, evaluates rules and
// CEP patterns entirely in memory, and snapshots periodically for fast recovery.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
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

	// Control plane lives in PostgreSQL only — rule_strategies + cep_patterns.
	// No Redis at all: the in-process atomic.Pointer cache in
	// RuleStrategyUsecase is sufficient for a single-process shard. The Redis
	// cache layer was removed in Task Q together with the go-redis dependency.
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
	shardID := envInt("SHARD_ID", 0)
	natsURL := envStr("NATS_URL", nats.DefaultURL)
	snapshotDir := envStr("SNAPSHOT_DIR", "")

	// Build this shard's engine and register its CEP patterns.
	engine := core.NewCore(shardID, ruleSet)
	for _, p := range patterns {
		if err := engine.AddPattern(p); err != nil {
			log.Fatal("add pattern: ", err)
		}
	}

	// Connect to NATS JetStream.
	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatal("connect nats: ", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		log.Fatal("jetstream: ", err)
	}

	natsCfg := core.NATSConfig{
		StreamName:       "rule-events",
		Subjects:         []string{"rule.events.>"},
		FilterSubject:    fmt.Sprintf("rule.events.%d.>", shardID),
		MaxAckPending:    1000,
		SnapshotInterval: 60 * time.Second,
	}
	if snapshotDir != "" {
		natsCfg.SnapshotPath = fmt.Sprintf("%s/shard_%d.snap", snapshotDir, shardID)
	}

	slog.Info("rule-engine-core starting",
		"shard", shardID, "backend", "nats", "nats", natsURL,
		"rules", len(ruleSet.Strategies), "patterns", len(patterns))

	consumer := core.NewNATSConsumer(engine, js, natsCfg)
	if err := consumer.Run(ctx); err != nil {
		log.Fatal("engine run: ", err)
	}
	slog.Info("rule-engine-core stopped", "shard", shardID)
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
