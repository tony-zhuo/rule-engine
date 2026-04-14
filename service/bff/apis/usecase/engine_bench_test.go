package usecase

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	behaviorRedis "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/redis"
	cepDB "github.com/tony-zhuo/rule-engine/service/base/cep/repository/db"
	cepRedis "github.com/tony-zhuo/rule-engine/service/base/cep/repository/redis"
	cepUsecase "github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
)

// setupIntegration wires up the full CheckEvent pipeline with real Redis and DB.
// Requires: docker compose up (PostgreSQL + Redis + Kafka)
func setupIntegration(tb testing.TB) *EngineUsecase {
	return setupIntegrationWithPool(tb, 0, 0)
}

func setupIntegrationWithPool(tb testing.TB, poolSize, minIdleConns int) *EngineUsecase {
	tb.Helper()

	// --- PostgreSQL (for loading rules & CEP patterns at startup only) ---
	pkgdb.Init(pkgdb.DBConfig{
		Host: "localhost", User: "rule_engine", Password: "rule_engine",
		DBName: "rule_engine", Port: 5432, SSLMode: "disable", TimeZone: "UTC",
	})
	db := pkgdb.GetDB()
	if db == nil {
		tb.Skip("DB not available, skipping integration benchmark")
	}

	// --- Redis (hot path: event store + rule cache + CEP state) ---
	opts := &goredis.Options{Addr: "localhost:6379"}
	if poolSize > 0 {
		opts.PoolSize = poolSize
	}
	if minIdleConns > 0 {
		opts.MinIdleConns = minIdleConns
	}
	rdb := goredis.NewClient(opts)
	tb.Logf("Redis pool: PoolSize=%d, MinIdleConns=%d", opts.PoolSize, opts.MinIdleConns)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		tb.Skip("Redis not available, skipping integration benchmark")
	}

	// Rule strategy usecase (loads from DB, caches in Redis)
	ruleRepo := ruleDB.NewRuleStrategyRepoWith(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecaseWith(ruleRepo, ruleUC, rdb)

	// Verify we have active rules
	ruleSet, err := ruleStrategyUC.ListActiveCompiled(context.Background())
	if err != nil {
		tb.Fatalf("failed to load compiled rules: %v", err)
	}
	tb.Logf("Loaded %d active compiled rules (maxWindow=%v)", len(ruleSet.Strategies), ruleSet.MaxWindow)

	// Redis event store (hot path)
	eventStore := behaviorRedis.NewBehaviorEventStore(rdb)

	// CEP processor (load patterns from DB, state in Redis)
	cepPatternRepo := cepDB.NewCEPPatternRepoWith(db)
	cepStore := cepRedis.NewRedisStore(rdb)
	cepUC := cepUsecase.NewCEPUsecaseWith(cepStore, ruleUC)

	patterns, err := cepPatternRepo.ListActive(context.Background())
	if err != nil {
		tb.Fatalf("failed to load CEP patterns: %v", err)
	}
	for _, p := range patterns {
		cepUC.AddPattern(p)
	}
	tb.Logf("Loaded %d CEP patterns", len(patterns))

	return &EngineUsecase{
		ruleStrategyUC: ruleStrategyUC,
		eventStore:     eventStore,
		cepProcessor:   cepUC,
		// producer is nil — fire-and-forget goroutine will log error but not crash
	}
}

func buildCheckEventReq(eventID, memberID string) *CheckEventReq {
	return &CheckEventReq{
		EventID:    eventID,
		MemberID:   memberID,
		Behavior:   behaviorModel.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": float64(150000), "target_address": "0xBENCH"},
		OccurredAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Integration benchmarks — real Redis, real DB rules, real CEP
// ---------------------------------------------------------------------------

// BenchmarkCheckEvent_Integration measures single-goroutine latency.
//
// Run: go test -bench=BenchmarkCheckEvent_Integration$ -benchtime=10s -count=3 ./service/bff/apis/usecase/
func BenchmarkCheckEvent_Integration(b *testing.B) {
	uc := setupIntegration(b)
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		req := buildCheckEventReq(
			fmt.Sprintf("bench-%d", time.Now().UnixNano()),
			"bench-member-1",
		)
		if _, err := uc.CheckEvent(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckEvent_Throughput_Integration measures events/sec scaling across goroutines.
//
// Run: go test -bench=BenchmarkCheckEvent_Throughput_Integration -benchtime=10s ./service/bff/apis/usecase/
func BenchmarkCheckEvent_Throughput_Integration(b *testing.B) {
	uc := setupIntegration(b)
	ctx := context.Background()

	for _, workers := range []int{1, 4, 8, 16, 32} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			b.SetParallelism(workers)
			var counter atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id := counter.Add(1)
					req := buildCheckEventReq(
						fmt.Sprintf("tp-%d-%d", time.Now().UnixNano(), id),
						fmt.Sprintf("member-%d", id%100),
					)
					if _, err := uc.CheckEvent(ctx, req); err != nil {
						b.Fatal(err)
					}
				}
			})
			throughput := float64(b.N) / b.Elapsed().Seconds()
			b.ReportMetric(throughput, "events/s")
		})
	}
}

// BenchmarkCheckEvent_PoolTuning compares throughput across different Redis pool configurations.
//
// Run: go test -bench=BenchmarkCheckEvent_PoolTuning -benchtime=10s ./service/bff/apis/usecase/
func BenchmarkCheckEvent_PoolTuning(b *testing.B) {
	configs := []struct {
		name         string
		poolSize     int
		minIdleConns int
	}{
		{"default", 0, 0},             // go-redis default: 10*NumCPU, 0 idle
		{"pool50-idle10", 50, 10},
		{"pool100-idle20", 100, 20},
		{"pool200-idle50", 200, 50},
	}

	for _, cfg := range configs {
		b.Run(cfg.name, func(b *testing.B) {
			uc := setupIntegrationWithPool(b, cfg.poolSize, cfg.minIdleConns)
			ctx := context.Background()

			for _, workers := range []int{1, 8, 32} {
				b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
					b.SetParallelism(workers)
					var counter atomic.Int64
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							id := counter.Add(1)
							req := buildCheckEventReq(
								fmt.Sprintf("pool-%s-%d-%d", cfg.name, time.Now().UnixNano(), id),
								fmt.Sprintf("member-%d", id%100),
							)
							if _, err := uc.CheckEvent(ctx, req); err != nil {
								b.Fatal(err)
							}
						}
					})
					throughput := float64(b.N) / b.Elapsed().Seconds()
					b.ReportMetric(throughput, "events/s")
				})
			}
		})
	}
}
