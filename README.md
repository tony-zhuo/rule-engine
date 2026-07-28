# rule-engine

A behavioral risk control engine in Go — a hand-built, Flink-style streaming runtime that evaluates fraud rules and multi-step behavioral patterns against a live event stream.

State lives in memory and is mutated by exactly one goroutine per shard, so the hot path performs **no external I/O at all**. Durability comes from event sourcing instead: the message log is the source of truth, and a crashed shard rebuilds by loading a snapshot and replaying the log from the offset that snapshot recorded.

The engine is **MQ-pluggable** across NATS JetStream and Kafka, with a shadow-traffic test proving both backends drive the state to byte-identical results.

---

## Status

```
service/engine/core/  21/21 tests passing

  7 CEP tests          multi-step sequences, window expiry, negative patterns
  6 ProcessEvent tests rule firing, idempotency, determinism, out-of-order, late events
  3 Snapshot tests     round-trip / replay safety / side-effect suppression
  2 NATS tests         end-to-end + crash recovery over an embedded NATS server
  2 Kafka tests        end-to-end + crash recovery over real Kafka (testcontainers)
★ 1 Shadow test        NATS and Kafka backends produce identical ShardState
```

```bash
make test-short   # skips Kafka + shadow tests, no Docker needed
make test         # full suite (needs Docker, or set KAFKA_BROKERS)
```

---

## Architecture

Two planes, deliberately separated:

```
   control plane                      data plane
 ┌───────────────┐            ┌──────────────────────┐
 │   cmd/apis    │            │  cmd/event-producer  │
 │  rule + CEP   │            │   (load generator)   │
 │  pattern CRUD │            └──────────┬───────────┘
 └───────┬───────┘                       │ publish
         │ writes                        ▼
         ▼                   ┌──────────────────────┐
   ┌───────────┐             │  NATS JetStream      │
   │PostgreSQL │             │  ── or ── Kafka      │
   └───────────┘             │  (pluggable backend) │
         │ read at startup   └──────────┬───────────┘
         │                              │ consume
         └─────────────────▶┌───────────▼──────────┐
                            │ cmd/rule-engine-core │
                            │  one shard, one      │──▶ matched rules
                            │  goroutine, no locks │    + patterns
                            └───────────┬──────────┘
                                        │ periodic
                                        ▼
                                   snapshot + offset
                                     (on disk)
```

The control plane writes rules; the engine reads them once at startup and then never touches PostgreSQL again on the event path.

### File map

```
service/engine/core/
  ├─ keygroup.go        member → key group (crc32 % 128) → shard
  ├─ state.go           ShardState / MemberState / BehaviorAgg / BucketData
  ├─ aggregation.go     time-bucketed aggregation over the member's events
  ├─ eval.go            rule evaluation via the AST compiler
  ├─ core.go            ProcessEvent orchestration + late-event side output
  ├─ watermark.go       per-shard event-time watermark + lateness
  ├─ cep.go             multi-step pattern matching
  ├─ negative_queue.go  deadline heap for "A happened, then NOT B within W"
  ├─ snapshot.go        gob serialization + Restore + replay-safe idempotency
  │
  ├─ consumer.go        EventConsumer interface + shared helpers
  ├─ consumer_nats.go   NATS JetStream pull consumer (embedded server in tests)
  ├─ consumer_kafka.go  Kafka pull consumer via franz-go (pure Go, no cgo)
  │
  ├─ producer.go        EventProducer interface
  ├─ producer_nats.go   NATS producer (shard → subject hierarchy)
  └─ producer_kafka.go  Kafka producer (shard → partition)

cmd/
  ├─ rule-engine-core/  engine shard entry point (NATS backend wired)
  ├─ event-producer/    load generator (BACKEND=nats|kafka)
  └─ apis/              rule-admin HTTP API (control plane)
```

### Design decisions worth interviewing on

| Decision | What it is | Why it matters |
|---------|-----------|---------------|
| **Hand-built Flink internals** | key groups · event-time + watermark · snapshot + replay · CEP | Borrows Flink's correctness primitives at single-binary scale, and keeps the runtime in Go and fully owned |
| **Single-writer principle** | one shard = one goroutine; state has no locks | Borrows LMAX Disruptor's idea; correctness without mutexes, predictable throughput |
| **Two-stage key mapping** | `member → key group (128) → shard`, not `member → shard` | Rescaling moves whole key groups between shards instead of rehashing every member |
| **MQ-pluggable** | `EventConsumer` / `EventProducer` interfaces; NATS + Kafka backends | Kafka is the production-grade default for finance; NATS is the low-latency alternative and runs in-process in tests |
| **Shadow traffic comparison** | one event stream → both backends → `reflect.DeepEqual(state)` passes | Structural proof that "pluggable" isn't rhetoric — both backends produce identical results |
| **Engine owns the source offset** | snapshot stores `LastSeq`; consumer resumes from `LastSeq+1`, ignoring broker cursors | Source position and state stay consistent across crashes; same idea on both backends |
| **Replay-safe idempotency** | `event_id` dedup co-located inside `BucketData`, snapshotted with the state | `Restore + replay overlap == single pass`, validated by `TestSnapshot_ReplaySafety` |
| **Negative patterns via a deadline heap** | "A, then NOT B within W" fires when the watermark passes the deadline | Timer-driven matching in an otherwise event-driven engine; a silent member still gets their match |

---

## Running it

```bash
make docker-up            # PostgreSQL + NATS + Kafka
make migrate              # create rule_strategies + cep_patterns, seed examples

make run-core             # terminal 1 — engine shard (NATS backend)
RATE=500 COUNT=5000 make run-producer   # terminal 2 — synthetic load

# Same load against Kafka instead:
BACKEND=kafka KAFKA_BROKERS=localhost:9092 make run-producer

make run-api              # optional — rule CRUD on :8080
```

`cmd/apis` reads `config.yaml`. The engine binaries are configured by environment variables:

| Binary | Variables |
|--------|-----------|
| `rule-engine-core` | `SHARD_ID`, `NATS_URL`, `SNAPSHOT_DIR` |
| `event-producer` | `BACKEND` (nats\|kafka), `TOPIC`, `NUM_SHARDS`, `RATE`, `COUNT`, `MEMBER_POOL`, `BEHAVIOR`, `NATS_URL` / `SUBJECT_PREFIX`, `KAFKA_BROKERS` |

---

## Design Documents

| Document | What's inside |
|----------|--------------|
| [`docs/in-memory-rule-engine-plan.md`](docs/in-memory-rule-engine-plan.md) | The full 2600-line plan: architecture, key groups, async barrier snapshotting, watermark + lateness, exactly-once sink (2PC), rescaling, hot-key handling, observability, benchmark roadmap, Flink comparison |
| [`docs/in-memory-rule-engine-gaps.md`](docs/in-memory-rule-engine-gaps.md) | Tracked design gaps with severity, rationale, and deferred decisions |
| [`docs/in-memory-rule-engine-references.md`](docs/in-memory-rule-engine-references.md) | Comparable products (Stripe Radar, Coinbase risk, LMAX) and recommended deep-reads |
| [`docs/in-memory-rule-engine-nats-vs-kafka.md`](docs/in-memory-rule-engine-nats-vs-kafka.md) | Backend selection writeup: why dual backend, why Kafka as the production default, common interview traps |

---

## Tech Stack

**Engine** — Go 1.25 · NATS JetStream (embedded server for tests) · franz-go (pure-Go Kafka client) · testcontainers-go · `encoding/gob` for snapshots
**Domain** — custom AST rule compiler · CEP pattern matcher
**Control plane** — Gin · GORM · PostgreSQL 17 · Google Wire · slog · pprof-ready

PostgreSQL holds `rule_strategies` and `cep_patterns` only — configuration, not event data. Event state lives in shard memory, and its durable record is the message log plus periodic snapshots.

---

## Roadmap

```
✅  Engine core, both MQ backends, shadow-verified state equivalence (21/21 green)
✅  Negative CEP patterns (timer-driven deadline heap)
⏳  Benchmark roadmap M1 → M5 (10K → 100K per shard → 500K across shards)
⏳  Async barrier checkpointing + per-key-group incremental snapshots
⏳  Multi-shard in one process + backpressure + per-shard metrics
⏳  Hot-key quarantine (gap #2)
⏳  Long-term archive pipeline (NATS → S3, gap #28)
```
