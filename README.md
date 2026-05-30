# rule-engine

A behavioral risk control engine in Go, evolved through **two architectures in the same repo**:

1. **Legacy pipeline** — synchronous API + Kafka worker + Redis-backed aggregation. Mirrors a typical production setup. Serves as the performance baseline.
2. **In-memory engine** ⭐ — hand-built Flink-style streaming runtime: event-sourced, key-group sharded, single-writer, snapshot+replay recoverable, **MQ-pluggable across NATS JetStream and Kafka** with shadow-traffic-verified state equivalence.

The in-memory rewrite is the technical depth piece; the legacy pipeline is what it improves on. Both live in the tree.

---

## Status

```
service/engine/core/  17/17 tests passing (ok 5.454s)
  3 CEP tests          multi-step fraud sequence detection
  2 Kafka tests        KafkaConsumer over real Kafka (testcontainers auto-starts)
  2 NATS tests         NATSConsumer over embedded NATS (in-process)
  3 ProcessEvent tests core engine logic
★ 1 Shadow test        NATS + Kafka backends produce byte-identical ShardState
  3 Snapshot tests     round-trip / replay safety / side-effect suppression
  3 Watermark tests    out-of-order / late-event handling
```

```bash
# Fast subset (skips Kafka / shadow)
go test -short ./service/engine/core/

# Full suite (needs Docker for testcontainers, or KAFKA_BROKERS env)
go test ./service/engine/core/
```

---

## ⭐ In-Memory Engine (the showcase)

### Architecture

```
                publish              consume
upstream ──────▶  NATS JetStream  ───────▶  Rule Engine Core  ─────────▶  matched results
                  Kafka            (one shard, single goroutine,         (snapshot to disk)
                  ↑ pluggable      no locks, event-time semantics)
                  ↑
         cmd/event-producer
         (BACKEND=nats|kafka,
          load generator)
```

### File map

```
service/engine/core/
  ├─ A. keygroup.go        member → key_group (crc32 % 128) → shard
  ├─ B. state.go           ShardState / MemberState / BehaviorAgg / BucketData
  ├─ C. aggregation.go     in-memory time-bucket aggregation (replaces Redis sorted set)
  ├─ D. eval.go            rule evaluation reusing the existing AST compiler
  ├─ E. core.go            ProcessEvent orchestration + late-event side output
  ├─ F. watermark.go       per-shard event-time watermark + lateness
  ├─ G. cep.go             in-memory multi-step pattern matching
  ├─ G. snapshot.go        gob serialization + Restore + replay-safe idempotency
  │
  ├─ consumer.go           EventConsumer interface + shared helpers
  ├─ consumer_nats.go      NATS JetStream pull consumer (embedded server in tests)
  ├─ consumer_kafka.go     Kafka pull consumer via franz-go (pure Go, no cgo)
  │
  ├─ producer.go           EventProducer interface
  ├─ producer_nats.go      NATS producer (shard → subject hierarchy)
  └─ producer_kafka.go     Kafka producer (shard → partition)

cmd/
  ├─ rule-engine-core/     in-memory engine entry point (NATS backend wired)
  └─ event-producer/       load generator (BACKEND=nats|kafka toggle)
```

### Design decisions worth interviewing on

| Decision | What it is | Why it matters |
|---------|-----------|---------------|
| **Hand-built Flink internals** | key groups · event-time + watermark · barrier-style snapshot · replay · CEP | Borrows Flink's correctness primitives at single-binary scale; not using Flink itself keeps the runtime in Go and fully owned |
| **Single-writer principle** | one shard = one goroutine; state has no locks | Borrows LMAX Disruptor's idea; correctness without mutexes, predictable throughput |
| **MQ-pluggable** | `EventConsumer` / `EventProducer` interfaces; NATS + Kafka backends | Production default = Kafka (finance-grade implementations at Coinbase / PayPal / ING); NATS = alternative for low-latency variant + in-process tests |
| **Shadow traffic comparison** | one event stream → both backends → `reflect.DeepEqual(state)` passes | Structural proof that "pluggable" isn't rhetoric — both backends produce identical results |
| **Engine owns the source offset** | snapshot stores `LastSeq`; consumer resumes from `LastSeq+1` ignoring broker cursors | Source position + state stay consistent across crashes; same idea on both backends |
| **Replay-safe idempotency** | `event_id` dedup co-located inside `BucketData` (snapshotted with state) | `Restore + replay overlap == single pass`, validated by `TestSnapshot_ReplaySafety` |

### Quick demo

```bash
# Terminal 1 — start the engine (NATS default backend, loads rules from PG)
go run ./cmd/rule-engine-core

# Terminal 2 — drive load (synthetic events)
RATE=500 COUNT=5000 go run ./cmd/event-producer

# Or push the same load to Kafka instead:
KAFKA_BROKERS=localhost:9092 BACKEND=kafka go run ./cmd/event-producer
```

---

## Legacy Pipeline (the baseline that motivated the rewrite)

The original architecture — still in the repo at `cmd/apis` + `cmd/worker` — is what the in-memory engine improves on:

- **`cmd/apis`** — Gin HTTP server. `POST /v1/events/check` synchronously runs rule eval + CEP against Redis-backed state. Also serves rule/pattern CRUD.
- **`cmd/worker`** — Kafka consumer that write-backs events to PostgreSQL for audit.

**Bottlenecks that drove the rewrite**:
- Per-event Redis I/O (2–3 round-trips for aggregation + CEP progress) caps throughput at ~10K events/s
- Sync HTTP path couples request latency to evaluation cost
- CEP progress in Redis = network hop per pattern step

The in-memory rewrite eliminates per-event external I/O, holds state lock-free in one goroutine per shard, and recovers via NATS replay + periodic snapshots. Plan target (per-shard 100K events/s) is documented in [docs/in-memory-rule-engine-plan.md §Benchmark Roadmap](docs/in-memory-rule-engine-plan.md).

The legacy paths (`cmd/apis` CheckEvent endpoint, `cmd/worker`) are scheduled for removal once the rewrite is the only path; tracked in the task list.

---

## Design Documents

| Document | What's inside |
|----------|--------------|
| [`docs/in-memory-rule-engine-plan.md`](docs/in-memory-rule-engine-plan.md) | The full 2500-line plan: architecture, key groups, async barrier snapshotting, watermark + lateness, exactly-once sink (2PC), rescaling, hot-key handling, observability, benchmark roadmap, Flink comparison |
| [`docs/in-memory-rule-engine-gaps.md`](docs/in-memory-rule-engine-gaps.md) | 28 tracked design gaps with severity, rationale, deferred decisions — production-grade rigour |
| [`docs/in-memory-rule-engine-references.md`](docs/in-memory-rule-engine-references.md) | Comparable products (Stripe Radar, Coinbase risk, LMAX) and recommended deep-reads |
| [`docs/in-memory-rule-engine-nats-vs-kafka.md`](docs/in-memory-rule-engine-nats-vs-kafka.md) | Full selection writeup: why dual backend, why Kafka as production default for finance, what NATS retains its alternative role on, common interview traps |

---

## Tech Stack

**Engine** — Go 1.25 · NATS JetStream (with embedded server for tests) · franz-go (pure-Go Kafka client) · testcontainers-go (Kafka in tests)
**Domain layer (reused by both engine + legacy)** — Custom AST rule compiler · CEP pattern matcher · `encoding/gob` for snapshots
**Tooling** — Google Wire DI · Gin (legacy HTTP only) · slog · pprof-ready

### What actually runs against what

| Storage | In-Memory Engine (`cmd/rule-engine-core`) | Legacy (`cmd/apis` + `cmd/worker`) |
|---------|------------------------------------------|-----------------------------------|
| **PostgreSQL 17** | ✓ `rule_strategies` + `cep_patterns` only (control plane) | ✓ + `behavior_logs` (audit) + `processed_events` (dedup) — those two migrations have been dropped, see note below |
| **Redis 7** | ✗ Not touched at runtime (rdb=nil; in-process `atomic.Pointer` cache is enough for one process per shard) | ✓ Rule cache + CEP progress + aggregation cache |
| **NATS JetStream** | ✓ Default backend (event log + checkpoint KV in one cluster) | — |
| **Kafka** | ✓ Alternative backend via franz-go (Coinbase/PayPal/ING-grade) | ✓ Worker consumes here |

**Note on dropped migrations**: `002_create_behavior_logs.up.sql` and `006_create_processed_events.up.sql` were removed — the in-memory engine never writes to these tables (audit is deferred to a separate NATS → S3 pipeline, [gap #28](docs/in-memory-rule-engine-gaps.md)). Legacy `cmd/apis` / `cmd/worker` paths that still reference them will fail at runtime against a fresh DB until they are themselves removed (Tasks M, N).

**Note on Redis in the binary**: even though the engine doesn't *use* Redis, `go-redis` is still *linked* into `cmd/rule-engine-core` because the shared `RuleStrategyUsecase` struct (also used by `cmd/apis`) holds a `*redis.Client` field. Fully removing the linkage means either splitting the usecase or introducing a `Cache` interface — left as a follow-up once Task M removes the legacy API consumer of that struct.

---

## Roadmap

```
Implementation:  ✅  All MQ-pluggable engine work complete (17/17 tests green)
Remaining:       ⏳  Strip legacy CheckEvent endpoint    (cmd/apis)
                 ⏳  Remove legacy worker + tidy confluent-kafka-go dep
                 ⏳  Benchmark roadmap M2 → M5 (50K → 100K → 500K events/s)
                 ⏳  Hot-key quarantine (gap #2 in plan)
                 ⏳  Long-term archive pipeline (NATS → S3, gap #28)
```
