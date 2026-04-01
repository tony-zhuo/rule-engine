package engine

import (
	"context"

	"github.com/tony-zhuo/rule-engine/ast"
	"github.com/tony-zhuo/rule-engine/cep"
)

// Engine combines stateless AST rule evaluation with stateful CEP pattern matching.
type Engine struct {
	patterns  []cep.CEPPattern
	processor *cep.Processor
}

// New creates an Engine backed by the provided ProgressStore.
func New(store cep.ProgressStore) *Engine {
	return &Engine{
		processor: cep.NewProcessor(store),
	}
}

// LoadPattern registers a CEP pattern with the engine.
func (e *Engine) LoadPattern(p cep.CEPPattern) {
	e.patterns = append(e.patterns, p)
	e.processor.AddPattern(p)
}

// EvaluateRule performs a single, stateless AST rule evaluation.
func (e *Engine) EvaluateRule(node ast.RuleNode, ctx ast.EvalContext) (bool, error) {
	return ast.Evaluate(node, ctx)
}

// ProcessEvent runs the event through the CEP processor and returns any completed pattern matches.
func (e *Engine) ProcessEvent(ctx context.Context, event *cep.Event) ([]*cep.MatchResult, error) {
	return e.processor.ProcessEvent(ctx, event)
}
