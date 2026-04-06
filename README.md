# rule-engine

A Go-based risk control system with AST rule evaluation and Complex Event Processing (CEP) for real-time behavioral event analysis.

## System Architecture

```
                                    ┌─────────────────────────────────────────────┐
                                    │              Infrastructure                  │
                                    │                                              │
  ┌──────────┐    POST /v1/events   │  ┌─────────────────────────────────────┐    │
  │  Client   │ ──────────────────► │  │            API Server               │    │
  └──────────┘                      │  │  ┌──────────┐   ┌───────────────┐   │    │
       │                            │  │  │Controller │──►│EngineUsecase  │   │    │
       │       GET/POST /v1/rules   │  │  └──────────┘   └───────┬───────┘   │    │
       └──────────────────────────► │  │                          │           │    │
                                    │  │            Generate UUID + Produce   │    │
                                    │  └──────────────────────────┼───────────┘    │
                                    │                             │                │
                                    │                             ▼                │
                                    │               ┌─────────────────────┐        │
                                    │               │   Kafka (events)    │        │
                                    │               │   acks=-1           │        │
                                    │               │   128 partitions    │        │
                                    │               └──────────┬──────────┘        │
                                    │                          │                   │
                                    │                          ▼                   │
                                    │  ┌─────────────────────────────────────┐     │
                                    │  │            Worker                    │     │
                                    │  │                                      │     │
                                    │  │  1. Log behavior (idempotent)        │     │
                                    │  │          │                           │     │
                                    │  │          ▼                           │     │
                                    │  │  2. Load active rules (cached)       │     │
                                    │  │          │                           │     │
                                    │  │          ▼                           │     │
                                    │  │  3. Preload aggregation queries      │     │
                                    │  │          │                           │     │
                                    │  │          ▼                           │     │
                                    │  │  4. Evaluate rules (AST)             │     │
                                    │  │          │                           │     │
                                    │  │          ▼                           │     │
                                    │  │  5. CEP pattern matching             │     │
                                    │  │          │                           │     │
                                    │  │          ▼                           │     │
                                    │  │  6. Produce results                  │     │
                                    │  └──────┬───────────────────┬───────────┘     │
                                    │         │                   │                 │
                                    │         ▼                   ▼                 │
                                    │  ┌─────────────┐   ┌───────────────┐         │
                                    │  │  PostgreSQL  │   │Kafka (results)│         │
                                    │  │             │   └───────────────┘         │
                                    │  │ behavior_logs│          │                  │
                                    │  │ rule_strategies         ▼                  │
                                    │  │ cep_patterns │   ┌─────────────┐          │
                                    │  └─────────────┘   │  Downstream  │          │
                                    │                     │  (TBD)      │          │
                                    │         ▲           └─────────────┘          │
                                    │         │                                    │
                                    │  ┌──────┴──────────────────────────┐         │
                                    │  │  Redis Sentinel Cluster         │         │
                                    │  │  Master + 2 Replica + 3 Sentinel│         │
                                    │  │                                 │         │
                                    │  │  - Rule cache (TTL 60s)         │         │
                                    │  │  - CEP progress state           │         │
                                    │  │  - AOF persistence              │         │
                                    │  └─────────────────────────────────┘         │
                                    └─────────────────────────────────────────────┘
```

### Rule Engine vs CEP

| | Rule Strategy | CEP Pattern |
|---|---|---|
| Trigger | Single event + historical aggregation | Multiple events in sequence |
| State | Stateless (queries DB) | Stateful (progress in Redis) |
| Ordering | Not guaranteed | Strictly enforced |
| Use case | Thresholds, frequency, cumulative amounts | Behavioral chains, flow anomaly detection |

### Infrastructure

| Component | Purpose | Config |
|---|---|---|
| PostgreSQL 17 | behavior_logs, rule_strategies, cep_patterns | Single instance |
| Redis 7 Sentinel | Rule cache, CEP progress state | 1 master + 2 replicas + 3 sentinels, AOF |
| Kafka (KRaft) | Event streaming between API and Worker | acks=-1, manual commit, sync delivery |

## Features

- **AST rule evaluation** — arbitrary `AND`/`OR`/`NOT` trees where each leaf is a condition with field, operator, value, and optional time-window
- **CEP pattern matching** — multi-step stateful sequences across events, with per-step `MaxWait` windows and cross-event variable binding
- **At-least-once delivery** — sync Kafka produce with `acks=-1`, manual consumer commit, idempotent writes via `event_id`
- **Graceful shutdown** — signal handling, in-flight request drain, producer flush
- **Redis Sentinel** — automatic failover with AOF persistence

## API Endpoints

### POST /v1/events
Submit a behavioral event for async processing.

```json
{
  "event_id": "evt-001",
  "member_id": "member-42",
  "behavior": "CryptoWithdraw",
  "fields": { "amount": 150000, "target_address": "0xABCD..." },
  "occurred_at": "2024-01-01T00:00:00Z"
}
```

Response: `202 Accepted` — event is produced to Kafka for async processing.

### GET /v1/rules
List rule strategies. Optional query: `?status=1` (active) or `?status=2` (inactive).

### POST /v1/rules
Create a rule strategy.

### GET /v1/rules/:id
Get a rule strategy by ID.

### PUT /v1/rules/:id
Update a rule strategy.

### PATCH /v1/rules/:id/status
Enable or disable a rule strategy.

## Supported Operators

| Operator | Types             |
|----------|-------------------|
| `>`      | numeric           |
| `<`      | numeric           |
| `>=`     | numeric           |
| `<=`     | numeric           |
| `=`      | numeric, string   |
| `!=`     | numeric, string   |
| `IN`     | any (string cast) |
| `NOT_IN` | any (string cast) |

## CEP Variable Binding

State conditions can reference variables captured by earlier states using `$variable_name` in the `Value` position:

```json
{
  "type": "CONDITION",
  "field": "target_address",
  "operator": "=",
  "value": "$deposit_source"
}
```

Variables are extracted per state via `ContextBinding` (`variable_name` → `"$event.field_path"`).

## Quick Start

```bash
# Start infrastructure
make docker-up

# Run API server (local)
make run-api

# Run worker (local)
make run-worker

# Run migrations manually (if needed)
make migrate
```

## Makefile Commands

| Command | Description |
|---|---|
| `make build` | Build API and worker binaries |
| `make run-api` | Run API server locally |
| `make run-worker` | Run worker locally |
| `make migrate` | Run SQL migrations |
| `make migrate-down` | Drop all tables |
| `make docker-up` | Start docker compose |
| `make docker-down` | Stop docker compose |
| `make docker-reset` | Destroy volumes and rebuild |
| `make kafka-topics` | List Kafka topics |
| `make kafka-consume` | Consume messages from events topic |
| `make kafka-lag` | Show consumer group lag |
| `make kafka-describe` | Describe events topic partitions |

## Benchmark

```bash
# Unit (no infra needed)
go test -bench=BenchmarkExecute_Unit -benchtime=10s ./service/bff/worker/usecase/

# Integration (requires docker compose up)
go test -bench=BenchmarkExecute_Integration -benchtime=10s ./service/bff/worker/usecase/
```

### Results (Apple M2 Pro)

| Benchmark | Avg Latency |
|---|---|
| Unit (6 rules, mocked I/O) | ~9 us |
| Unit (300 rules, mocked I/O) | ~194 us |
| Integration (6 rules, real DB/Redis/Kafka) | ~9 ms |
| Integration (high aggregation) | ~9.6 ms |

## TODO

- [ ] 每秒 10,000 event 時的效能
- [ ] Kafka partition 數量調整（benchmark 建議 128）
- [ ] Production: `replication.factor=3`, `min.insync.replicas=2`
