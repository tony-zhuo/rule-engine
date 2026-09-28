// Command bench measures end-to-end latency (publish → ProcessEvent done) and
// throughput of the rule engine over a chosen MQ backend. Producer and consumer
// run in the SAME process so latency is computed against one clock.
//
// Modes (MODE env):
//   - fixed  (default): produce at RATE for DURATION, report the latency
//     distribution and achieved throughput.
//   - ladder: run stages at increasing rates (RATES env) until a stage fails —
//     either the producer can't sustain the target rate (producer-bound) or the
//     consumer can't drain within grace (consumer/MQ-bound). Reports per-stage.
//
// Each run uses a fresh, uniquely-named stream/topic so nothing is ever
// replayed — every measured event is live (OnProcessed skips replay anyway).
//
// Publishing is async (jetstream.PublishAsync / kgo.Produce) — the sync
// EventProducer interface tops out well below the rates measured here. Events
// carry OccurredAt = enqueue wall time; the producer paces in 5ms batches
// against an absolute schedule, and a stage whose achieved rate falls under
// 99% of target is flagged instead of silently reporting flattering latencies
// (coordinated-omission guard).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/pkg/env"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepDB "github.com/tony-zhuo/rule-engine/service/base/cep/repository/db"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	"github.com/tony-zhuo/rule-engine/service/engine/core"
)

// collector receives OnProcessed latencies. Single-writer (the consumer
// goroutine) but read from the bench goroutine, hence the mutex.
type collector struct {
	mu        sync.Mutex
	durs      []time.Duration
	processed atomic.Int64 // cumulative across all stages
}

func (c *collector) add(d time.Duration) {
	c.mu.Lock()
	c.durs = append(c.durs, d)
	c.mu.Unlock()
	c.processed.Add(1)
}

// drainSegment returns and clears the current segment's samples.
func (c *collector) drainSegment() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.durs
	c.durs = nil
	return out
}

// asyncPublisher is the bench-local publish fast path (async, counted).
type asyncPublisher struct {
	publish func(ev *behaviorModel.BehaviorEvent) // enqueue, non-blocking
	flush   func() error                          // wait for all pending acks
	errs    *atomic.Int64                         // failed publishes
}

type stageResult struct {
	TargetRate   int
	AchievedRate float64
	Produced     int64
	PubErrors    int64
	Drained      bool
	DrainTime    time.Duration
	Latency      summary
	samples      []time.Duration // raw latencies, for CSV output
}

func main() {
	ctx := context.Background()

	// --- knobs ---
	backend := env.Str("BACKEND", "nats")
	mode := env.Str("MODE", "fixed")
	rate := env.Int("RATE", 10000)
	duration := env.Duration("DURATION", 60*time.Second)
	memberPool := env.Int("MEMBER_POOL", 100)
	snapshotOn := env.Str("SNAPSHOT", "off") == "on"
	outDir := env.Str("OUT_DIR", "docs/benchmarks/raw")
	ratesCSV := env.Str("RATES", "10000,20000,40000,80000,160000")

	runID := fmt.Sprintf("bench-%d", time.Now().Unix())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal("mkdir out dir: ", err)
	}

	// --- real rule set from PG (the eval path being benchmarked) ---
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("load config: ", err)
	}
	pkgdb.Init(cfg.DB)
	db := pkgdb.GetDB()
	strategyUC := ruleUsecase.NewRuleStrategyUsecase(
		ruleDB.NewRuleStrategyRepo(db), ruleUsecase.NewRuleUsecase(),
	)
	ruleSet, err := strategyUC.ListActiveCompiled(ctx)
	if err != nil {
		log.Fatal("load compiled rules: ", err)
	}
	patterns, err := cepDB.NewCEPPatternRepo(db).ListActive(ctx)
	if err != nil {
		log.Fatal("load cep patterns: ", err)
	}

	engine := core.NewCore(0, ruleSet)
	for _, p := range patterns {
		if err := engine.AddPattern(p); err != nil {
			log.Fatal("add pattern: ", err)
		}
	}

	snapshotPath := ""
	if snapshotOn {
		snapshotPath = filepath.Join(os.TempDir(), runID+".snap")
	}

	// --- backend wiring: fresh stream/topic named after the run ---
	col := &collector{}
	var pub asyncPublisher
	var consumer core.EventConsumer
	var cleanup func()
	switch backend {
	case "nats":
		pub, consumer, cleanup = setupNATS(ctx, engine, runID, snapshotPath, col)
	case "kafka":
		pub, consumer, cleanup = setupKafka(ctx, engine, runID, snapshotPath, col)
	default:
		log.Fatalf("unknown BACKEND=%q (want: nats|kafka)", backend)
	}
	defer cleanup()

	runCtx, stopConsumer := context.WithCancel(ctx)
	consumerDone := make(chan error, 1)
	go func() { consumerDone <- consumer.Run(runCtx) }()
	time.Sleep(500 * time.Millisecond) // let the consumer pin its position before stage 1

	slog.Info("bench starting", "run", runID, "backend", backend, "mode", mode,
		"snapshot", snapshotOn, "rules", len(ruleSet.Strategies), "patterns", len(patterns),
		"member_pool", memberPool)

	// --- run stages ---
	var stages []stageResult
	switch mode {
	case "fixed":
		stages = append(stages, runStage(pub, col, rate, duration, memberPool))
	case "ladder":
		for _, rs := range strings.Split(ratesCSV, ",") {
			r, cerr := strconv.Atoi(strings.TrimSpace(rs))
			if cerr != nil || r <= 0 {
				log.Fatalf("bad RATES entry %q", rs)
			}
			res := runStage(pub, col, r, duration, memberPool)
			stages = append(stages, res)
			if !res.Drained || res.AchievedRate < 0.99*float64(res.TargetRate) {
				slog.Info("ladder stopping: stage failed", "target_rate", r)
				break
			}
		}
	default:
		log.Fatalf("unknown MODE=%q (want: fixed|ladder)", mode)
	}

	stopConsumer()
	if err := <-consumerDone; err != nil {
		slog.Warn("consumer exited with error", "error", err)
	}

	// --- report ---
	fmt.Printf("\nrun=%s backend=%s snapshot=%v member_pool=%d\n", runID, backend, snapshotOn, memberPool)
	fmt.Printf("%-12s %-12s %-10s %-9s %-10s %-10s %-10s %-10s %-10s %s\n",
		"target_eps", "achieved", "produced", "puberr", "p50", "p95", "p99", "max", "mean", "drained(in)")
	for i, s := range stages {
		csvPath := filepath.Join(outDir, fmt.Sprintf("%s-stage%d-%s.csv", runID, i, backend))
		if err := writeSamplesCSV(csvPath, s.samples); err != nil {
			slog.Warn("write raw csv", "path", csvPath, "error", err)
		}
		fmt.Printf("%-12d %-12.0f %-10d %-9d %-10v %-10v %-10v %-10v %-10v %v(%v)\n",
			s.TargetRate, s.AchievedRate, s.Produced, s.PubErrors,
			s.Latency.P50, s.Latency.P95, s.Latency.P99, s.Latency.Max, s.Latency.Mean,
			s.Drained, s.DrainTime.Round(time.Millisecond))
		fmt.Printf("  raw: %s\n", csvPath)
	}
}

// writeSamplesCSV writes one latency (nanoseconds) per line.
func writeSamplesCSV(path string, samples []time.Duration) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, d := range samples {
		fmt.Fprintln(w, d.Nanoseconds())
	}
	return w.Flush()
}

// runStage produces at targetRate for duration, waits for the consumer to
// drain, and returns the stage's stats. Writes nothing itself — the caller
// owns CSV output via the returned summary's samples living in the collector.
func runStage(pub asyncPublisher, col *collector, targetRate int, duration time.Duration, memberPool int) stageResult {
	col.drainSegment() // discard anything from a previous stage's drain tail
	pubErrsBefore := pub.errs.Load()
	processedBefore := col.processed.Load()

	slog.Info("stage starting", "target_eps", targetRate, "duration", duration)

	const tick = 5 * time.Millisecond
	perTick := float64(targetRate) * tick.Seconds()
	start := time.Now()
	var produced int64
	var schedIdx float64

	for time.Since(start) < duration {
		// Absolute schedule: by elapsed time T we should have sent rate*T events.
		target := float64(targetRate) * time.Since(start).Seconds()
		for schedIdx < target {
			ev := &behaviorModel.BehaviorEvent{
				EventID:    uuid.NewString(),
				MemberID:   fmt.Sprintf("user-%04d", int(produced)%memberPool+1),
				Behavior:   behaviorModel.BehaviorCryptoWithdraw,
				Fields:     map[string]any{"amount": float64(int(produced)%10000) + 0.5},
				OccurredAt: time.Now(),
			}
			pub.publish(ev)
			produced++
			schedIdx++
		}
		// Sleep to the next tick boundary; oversleep is fine — the absolute
		// schedule catches us up on the next iteration (no rate drift).
		time.Sleep(tick)
		_ = perTick
	}
	elapsed := time.Since(start)

	if err := pub.flush(); err != nil {
		slog.Warn("flush", "error", err)
	}
	pubErrs := pub.errs.Load() - pubErrsBefore
	okProduced := produced - pubErrs

	// Drain: wait until the consumer has processed everything this stage sent.
	grace := 15 * time.Second
	drainStart := time.Now()
	drained := false
	for time.Since(drainStart) < grace {
		if col.processed.Load()-processedBefore >= okProduced {
			drained = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	samples := col.drainSegment()
	res := stageResult{
		TargetRate:   targetRate,
		AchievedRate: float64(produced) / elapsed.Seconds(),
		Produced:     produced,
		PubErrors:    pubErrs,
		Drained:      drained,
		DrainTime:    time.Since(drainStart),
		Latency:      summarize(samples),
		samples:      samples,
	}
	slog.Info("stage done", "target_eps", targetRate, "achieved", fmt.Sprintf("%.0f", res.AchievedRate),
		"p99", res.Latency.P99, "drained", drained)
	return res
}

// setupNATS creates a run-scoped stream and returns an async publisher, the
// engine consumer bound to that stream, and a cleanup closure.
func setupNATS(ctx context.Context, engine *core.Core, runID, snapshotPath string, col *collector) (asyncPublisher, core.EventConsumer, func()) {
	natsURL := env.Str("NATS_URL", nats.DefaultURL)
	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatal("connect nats: ", err)
	}
	var pubErrs atomic.Int64
	js, err := jetstream.New(nc,
		jetstream.WithPublishAsyncMaxPending(8192),
		jetstream.WithPublishAsyncErrHandler(func(_ jetstream.JetStream, _ *nats.Msg, _ error) {
			pubErrs.Add(1)
		}),
	)
	if err != nil {
		nc.Close()
		log.Fatal("jetstream: ", err)
	}
	prefix := runID // subject namespace: <runID>.<shard>.<member>
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:     runID,
		Subjects: []string{prefix + ".>"},
	}); err != nil {
		nc.Close()
		log.Fatal("create stream: ", err)
	}

	enc := func(ev *behaviorModel.BehaviorEvent) {
		subject := fmt.Sprintf("%s.0.%s", prefix, ev.MemberID)
		data, _ := json.Marshal(ev)
		if _, perr := js.PublishAsync(subject, data); perr != nil {
			pubErrs.Add(1)
		}
	}
	flush := func() error {
		select {
		case <-js.PublishAsyncComplete():
			return nil
		case <-time.After(30 * time.Second):
			return fmt.Errorf("nats: async publish flush timed out")
		}
	}

	consumer := core.NewNATSConsumer(engine, js, core.NATSConfig{
		StreamName:       runID,
		Subjects:         []string{prefix + ".>"},
		FilterSubject:    prefix + ".0.>",
		MaxAckPending:    8192,
		SnapshotPath:     snapshotPath,
		SnapshotInterval: 10 * time.Second,
		OnProcessed:      col.add,
	})
	cleanup := func() {
		_ = js.DeleteStream(context.Background(), runID)
		nc.Close()
	}
	return asyncPublisher{publish: enc, flush: flush, errs: &pubErrs}, consumer, cleanup
}

// setupKafka creates a run-scoped single-partition topic and returns an async
// publisher, the engine consumer pinned to partition 0, and a cleanup closure.
func setupKafka(ctx context.Context, engine *core.Core, runID, snapshotPath string, col *collector) (asyncPublisher, core.EventConsumer, func()) {
	brokers := strings.Split(env.Str("KAFKA_BROKERS", "localhost:9092"), ",")

	// RecordDeliveryTimeout turns a stalled produce (e.g. metadata churn while a
	// previous run's multi-million-record topic is still being deleted) into a
	// counted pubErr instead of blocking the producer loop forever.
	producerClient, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.RecordDeliveryTimeout(30*time.Second),
	)
	if err != nil {
		log.Fatal("kafka producer client: ", err)
	}
	adm := kadm.NewClient(producerClient)
	if _, err := adm.CreateTopics(ctx, 1, 1, nil, runID); err != nil {
		slog.Warn("create topic (best-effort)", "topic", runID, "error", err)
	}
	// Let topic creation (and any prior run's async topic deletion) settle
	// before the first stage floods the fresh topic.
	time.Sleep(3 * time.Second)

	consumerClient, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		log.Fatal("kafka consumer client: ", err)
	}

	var pubErrs atomic.Int64
	enc := func(ev *behaviorModel.BehaviorEvent) {
		data, _ := json.Marshal(ev)
		producerClient.Produce(ctx, &kgo.Record{
			Topic:     runID,
			Partition: 0,
			Key:       []byte(ev.MemberID),
			Value:     data,
		}, func(_ *kgo.Record, perr error) {
			if perr != nil {
				pubErrs.Add(1)
			}
		})
	}
	flush := func() error { return producerClient.Flush(ctx) }

	consumer := core.NewKafkaConsumer(engine, consumerClient, core.KafkaConfig{
		Topic:            runID,
		Partition:        0,
		MaxPollRecords:   8192,
		SnapshotPath:     snapshotPath,
		SnapshotInterval: 10 * time.Second,
		OnProcessed:      col.add,
	})
	cleanup := func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = adm.DeleteTopics(delCtx, runID)
		producerClient.Close()
		consumerClient.Close()
	}
	return asyncPublisher{publish: enc, flush: flush, errs: &pubErrs}, consumer, cleanup
}
