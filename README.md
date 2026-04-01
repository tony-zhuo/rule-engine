# rule-engine

A Go library for AST-based rule evaluation and Complex Event Processing (CEP) with stateful cross-event pattern matching.

## Features

- **AST rule evaluation** — arbitrary `AND`/`OR`/`NOT` trees where each leaf is a condition with field, operator, value, and optional time-window
- **CEP pattern matching** — multi-step stateful sequences across events, with per-step `MaxWait` windows and cross-event variable binding
- **Pluggable storage** — `MemoryStore` for tests, `RedisStore` for production

## Project Structure

```
rule-engine/
├── ast/
│   ├── node.go        # NodeType, RuleNode, TimeWindow
│   ├── context.go     # EvalContext interface + MapContext
│   └── evaluator.go   # Recursive Evaluate()
├── cep/
│   ├── pattern.go     # CEPPattern, PatternState, Event, MatchResult
│   ├── processor.go   # Processor.ProcessEvent()
│   └── store.go       # ProgressStore, MemoryStore, RedisStore
├── engine/
│   └── engine.go      # Engine — combines AST + CEP
└── examples/
    └── main.go        # Demo: crypto rule + deposit-then-withdraw CEP
```

## Quick Start

```go
import (
    "github.com/tony-zhuo/rule-engine/ast"
    "github.com/tony-zhuo/rule-engine/cep"
    "github.com/tony-zhuo/rule-engine/engine"
)

// Stateless AST rule
rule := ast.RuleNode{
    Type: ast.NodeAnd,
    Children: []ast.RuleNode{
        {Type: ast.NodeCondition, Field: "amount", Operator: ">", Value: float64(1_000_000)},
        {Type: ast.NodeCondition, Field: "count_3d", Operator: ">", Value: float64(5)},
    },
}
ctx := ast.NewMapContext(map[string]any{"amount": float64(2_000_000), "count_3d": float64(8)})
matched, err := ast.Evaluate(rule, ctx)

// CEP: deposit then withdraw to same address within 10 minutes
store := cep.NewMemoryStore()
eng := engine.New(store)
eng.LoadPattern(cep.CEPPattern{ /* see examples/main.go */ })
results, err := eng.ProcessEvent(context.Background(), event)
```

## Supported Operators

| Operator | Types         |
|----------|---------------|
| `>`      | numeric       |
| `<`      | numeric       |
| `>=`     | numeric       |
| `<=`     | numeric       |
| `=`      | numeric, string |
| `!=`     | numeric, string |
| `IN`     | any (compared as strings) |
| `NOT_IN` | any (compared as strings) |

## CEP Variable Binding

State conditions can reference variables captured by earlier states using `$variable_name` syntax in either the `Field` or `Value` position:

```json
{
  "type": "CONDITION",
  "field": "target_address",
  "operator": "=",
  "value": "$deposit_source"
}
```

Variables are extracted per state via `ContextBinding` (`variable_name` → `"$event.field_path"`).

## Redis Store

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store := cep.NewRedisStore(rdb)
eng := engine.New(store)
```

Progress records are stored with a TTL derived from the pattern's `MaxWait` windows.

## Running the Example

```bash
go run ./examples/main.go
```
