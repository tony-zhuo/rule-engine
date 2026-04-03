# rule-engine

A Go service for AST-based rule evaluation and Complex Event Processing (CEP) with stateful cross-event pattern matching.

## Features

- **AST rule evaluation** — arbitrary `AND`/`OR`/`NOT` trees where each leaf is a condition with field, operator, value, and optional time-window
- **CEP pattern matching** — multi-step stateful sequences across events, with per-step `MaxWait` windows and cross-event variable binding
- **Pluggable storage** — `MemoryStore` for tests, `RedisStore` for production
- **HTTP API** — Gin-based REST endpoints via the engine BFF layer

## Project Structure

```
rule-engine/
├── cmd/
│   └── server/main.go              # Entry point
├── config/
│   └── config.go                   # App, Redis, Log config structs
├── pkg/
│   ├── logs/                       # slog wrapper
│   └── redis/                      # go-redis singleton
├── service/
│   ├── base/
│   │   ├── rule/                   # AST rule evaluation domain
│   │   │   ├── model/              # RuleNode, NodeType, EvalContext, MapContext
│   │   │   ├── usecase/            # Evaluate() + RuleUsecase
│   │   │   └── wire.go
│   │   └── cep/                    # CEP stateful pattern matching domain
│   │       ├── model/              # CEPPattern, Event, MatchResult, interfaces
│   │       ├── repository/
│   │       │   ├── memory/         # MemoryStore (for tests)
│   │       │   └── redis/          # RedisStore (for production)
│   │       ├── usecase/            # CEPUsecase (Processor)
│   │       └── wire.go
│   └── apis/
│       └── engine/                 # BFF — orchestrates rule + cep domains
│           ├── initialize/         # Conf struct
│           ├── controller/         # Gin handlers
│           ├── usecase/            # EngineUsecase
│           ├── router/             # Route registration
│           ├── wire/               # Wire DI (wire.go + wire_gen.go)
│           ├── init.go
│           └── router.go
└── examples/
    └── main.go                     # Demo: AST rule + deposit-then-withdraw CEP
```

## Data Flow

```
HTTP Request
  → router/      (route registration)
  → controller/  (bind request)
  → BFF usecase/ (orchestrate)
  → base rule/cep usecase/ (business logic)
  → repository/  (state storage)
  → Response
```

## API Endpoints

### POST /v1/engine/evaluate
Evaluate a stateless AST rule against a field map.

```json
{
  "rule": {
    "type": "AND",
    "children": [
      { "type": "CONDITION", "field": "amount", "operator": ">", "value": 1000000 },
      { "type": "CONDITION", "field": "count_3d", "operator": ">", "value": 5 }
    ]
  },
  "fields": {
    "amount": 2000000,
    "count_3d": 8
  }
}
```

Response: `{ "matched": true }`

### POST /v1/engine/events
Process a behavioral event through all registered CEP patterns.

```json
{
  "member_id": "member-42",
  "platform_id": "platform-1",
  "behavior": "CryptoWithdraw",
  "fields": { "target_address": "0xABCD..." },
  "occurred_at": "2024-01-01T00:00:00Z"
}
```

Response: `{ "matches": [...] }`

## Quick Start

```go
import (
    ruleModel    "github.com/tony-zhuo/rule-engine/service/base/rule/model"
    ruleUsecase  "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
    cepModel     "github.com/tony-zhuo/rule-engine/service/base/cep/model"
    cepMemory    "github.com/tony-zhuo/rule-engine/service/base/cep/repository/memory"
    cepUsecase   "github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
    engineUsecase "github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
)

// Wire up dependencies
store   := cepMemory.NewMemoryStore()
ruleUC  := ruleUsecase.NewRuleUsecase()
cepUC   := cepUsecase.NewCEPUsecase(store, ruleUC)
eng     := engineUsecase.NewEngineUsecase(ruleUC, cepUC)

// Stateless AST rule
rule := ruleModel.RuleNode{
    Type: ruleModel.NodeAnd,
    Children: []ruleModel.RuleNode{
        {Type: ruleModel.NodeCondition, Field: "amount", Operator: ">", Value: float64(1_000_000)},
        {Type: ruleModel.NodeCondition, Field: "count_3d", Operator: ">", Value: float64(5)},
    },
}
ctx := ruleModel.NewMapContext(map[string]any{"amount": float64(2_000_000), "count_3d": float64(8)})
matched, err := eng.EvaluateRule(rule, ctx)

// CEP: load pattern then process events
eng.LoadPattern(cepModel.CEPPattern{ /* see examples/main.go */ })
results, err := eng.ProcessEvent(context.Background(), &event)
```

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

## Running the Example

```bash
go run ./examples/main.go
```

## Starting the Server

```bash
go run ./cmd/server/main.go
# Listens on :8080
```

## TODO

- [ ] 驗證每個場景的交易行為
- [ ] 每秒 10,000 event 時的效能
