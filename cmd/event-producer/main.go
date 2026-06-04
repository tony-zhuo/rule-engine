// Command event-producer is a load generator and dev utility that publishes
// synthetic BehaviorEvents to the engine's MQ. Used for:
//   - Benchmark roadmap M1–M5 (plan §Benchmark Roadmap): drive controlled load
//   - Shadow traffic comparison (Task L): publish identical streams to both backends
//   - Dev / demo: send a few test events manually
//
// Backend is selected via BACKEND env (nats|kafka, default nats); the EventProducer
// interface (producer.go) keeps the main loop backend-agnostic.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	"github.com/tony-zhuo/rule-engine/service/engine/core"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Shared config (both backends).
	backend := envStr("BACKEND", "nats")
	topic := envStr("TOPIC", "rule-events") // also used as NATS stream name
	numShards := envInt("NUM_SHARDS", 1)
	rate := envInt("RATE", 100) // events/s
	count := envInt("COUNT", 0) // total events; 0 = run until cancelled
	memberPool := envInt("MEMBER_POOL", 100)
	behavior := envStr("BEHAVIOR", string(behaviorModel.BehaviorCryptoWithdraw))

	var (
		producer core.EventProducer
		shutdown func()
	)
	switch backend {
	case "nats":
		producer, shutdown = setupNATSProducer(ctx, topic, numShards)
	case "kafka":
		producer, shutdown = setupKafkaProducer(ctx, topic, numShards)
	default:
		log.Fatalf("unknown BACKEND=%q (want: nats|kafka)", backend)
	}
	defer shutdown()
	defer producer.Close()

	slog.Info("event-producer starting",
		"backend", backend, "topic", topic, "shards", numShards,
		"rate", rate, "count", count, "member_pool", memberPool, "behavior", behavior)

	// Rate limiter: one tick per event interval. Adequate sub-10K events/s;
	// beyond that, switch to batched producing (per-backend tuning).
	interval := time.Second / time.Duration(rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var sent int
	start := time.Now()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for {
		select {
		case <-ctx.Done():
			slog.Info("event-producer stopped",
				"sent", sent, "elapsed", time.Since(start).Round(time.Millisecond))
			return
		case now := <-ticker.C:
			memberID := fmt.Sprintf("user-%04d", rng.Intn(memberPool)+1)
			// EventID is generated here at event-creation time and stays with the
			// event for its entire lifecycle — that's the Event Identity Contract
			// (docs/in-memory-rule-engine-plan.md §Event Identity Contract). A
			// production producer SDK should use uuid.NewV7() (time-sortable),
			// reuse the same ID on retry, and never let any intermediary
			// regenerate it. uuid.NewString() (v4) is sufficient for this demo
			// load generator.
			event := &behaviorModel.BehaviorEvent{
				EventID:    uuid.NewString(),
				MemberID:   memberID,
				Behavior:   behaviorModel.BehaviorType(behavior),
				Fields:     map[string]any{"amount": rng.Float64() * 10000},
				OccurredAt: now,
			}
			if err := producer.Publish(ctx, event); err != nil {
				slog.Warn("publish failed", "event_id", event.EventID, "error", err)
				continue
			}
			sent++
			if count > 0 && sent >= count {
				slog.Info("event-producer finished",
					"sent", sent, "elapsed", time.Since(start).Round(time.Millisecond))
				return
			}
		}
	}
}

// setupNATSProducer connects to NATS, ensures the stream exists, and returns a
// producer + shutdown closure for the NATS connection.
func setupNATSProducer(ctx context.Context, stream string, numShards int) (core.EventProducer, func()) {
	natsURL := envStr("NATS_URL", nats.DefaultURL)
	subjectPrefix := envStr("SUBJECT_PREFIX", "rule.events")

	nc, err := nats.Connect(natsURL)
	if err != nil {
		log.Fatal("connect nats: ", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		log.Fatal("jetstream: ", err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     stream,
		Subjects: []string{subjectPrefix + ".>"},
	}); err != nil {
		nc.Close()
		log.Fatal("ensure stream: ", err)
	}
	return core.NewNATSProducer(js, core.NATSProducerConfig{
			SubjectPrefix: subjectPrefix,
			NumShards:     numShards,
		}),
		func() { nc.Close() }
}

// setupKafkaProducer connects to Kafka, best-effort creates the topic with
// NumShards partitions, and returns a producer + shutdown closure.
func setupKafkaProducer(ctx context.Context, topic string, numShards int) (core.EventProducer, func()) {
	brokers := strings.Split(envStr("KAFKA_BROKERS", "localhost:9092"), ",")
	client, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		log.Fatal("kafka client: ", err)
	}

	// Best-effort topic creation; ignore "already exists" — surfaces as a
	// per-topic error in the response, not the top-level error.
	adm := kadm.NewClient(client)
	createCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := adm.CreateTopics(createCtx, int32(numShards), 1, nil, topic); err != nil {
		slog.Warn("ensure topic (best-effort)", "topic", topic, "error", err)
	}

	return core.NewKafkaProducer(client, core.KafkaProducerConfig{
			Topic:     topic,
			NumShards: numShards,
		}),
		func() { client.Close() }
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
