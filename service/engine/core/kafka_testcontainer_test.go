package core

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	tckafka "github.com/testcontainers/testcontainers-go/modules/kafka"
)

// Container is started lazily via sync.Once on first use and reused across all
// Kafka + shadow tests in this package — starting it per-test would cost
// ~20s × N tests.
var (
	sharedKafkaOnce    sync.Once
	sharedKafkaBrokers []string
)

// kafkaBrokers returns Kafka brokers for integration tests. Lookup order:
//
//  1. `-short` test flag           → skip (these tests are never instant).
//  2. KAFKA_BROKERS env (comma-separated host:port) → use directly. Fastest
//     path when you already have Kafka running locally — no container churn.
//  3. testcontainers auto-starts a Kafka container (KRaft mode, Docker required).
//     Shared across all Kafka tests in this package via sync.Once.
//  4. Container start fails (no Docker, image pull error, ...) → skip with a
//     clear message rather than failing the suite.
//
// Reflects nats-vs-kafka.md §3.4: Kafka cannot be embedded in-process the way
// NATS can (consumer_test.go uses an embedded NATS server). The container hop
// is the real cost of "no in-process Kafka".
func kafkaBrokers(t *testing.T) []string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping Kafka integration test in -short mode")
	}
	if v := os.Getenv("KAFKA_BROKERS"); v != "" {
		return strings.Split(v, ",")
	}
	sharedKafkaOnce.Do(func() {
		ctx := context.Background()
		// WithClusterID is required for cp-kafka in KRaft mode — without it the
		// container exits with "CLUSTER_ID is required."
		container, err := tckafka.Run(ctx,
			"confluentinc/cp-kafka:7.6.0",
			tckafka.WithClusterID("rule-engine-test"),
		)
		if err != nil {
			slog.Warn("testcontainers: could not start kafka", "error", err)
			return
		}
		brokers, err := container.Brokers(ctx)
		if err != nil {
			slog.Warn("testcontainers: could not get brokers", "error", err)
			return
		}
		sharedKafkaBrokers = brokers
		// We deliberately don't Terminate — testcontainers' Reaper sidecar
		// cleans up on process exit, and reusing the container is what makes
		// running multiple Kafka tests bearable.
	})
	if len(sharedKafkaBrokers) == 0 {
		t.Skip("Kafka unavailable: set KAFKA_BROKERS or ensure Docker is running")
	}
	return sharedKafkaBrokers
}
