package model

import "context"

// EvalContext provides field resolution and cross-event variable access.
type EvalContext interface {
	Resolve(field string, window *TimeWindow) (any, error)
	GetVariable(name string) (any, bool)
}

// RuleUsecaseInterface is the contract for the rule evaluation usecase.
type RuleUsecaseInterface interface {
	Evaluate(node RuleNode, ctx EvalContext) (bool, error)
}

// RuleStrategyRepoInterface is the persistence contract for rule strategies.
type RuleStrategyRepoInterface interface {
	Get(ctx context.Context, id uint64) (*RuleStrategy, error)
	List(ctx context.Context, status *RuleStrategyStatus) ([]*RuleStrategy, error)
	Create(ctx context.Context, obj *RuleStrategy) error
	Update(ctx context.Context, id uint64, updates map[string]any) error
}

// RuleStrategyUsecaseInterface is the business-logic contract for rule strategies.
type RuleStrategyUsecaseInterface interface {
	Get(ctx context.Context, id uint64) (*RuleStrategy, error)
	List(ctx context.Context, status *RuleStrategyStatus) ([]*RuleStrategy, error)
	Create(ctx context.Context, req *CreateRuleStrategyReq) (*RuleStrategy, error)
	Update(ctx context.Context, id uint64, req *UpdateRuleStrategyReq) error
	SetStatus(ctx context.Context, id uint64, status RuleStrategyStatus) error
	ListActive(ctx context.Context) ([]*RuleStrategy, error)
	ListActiveCompiled(ctx context.Context) ([]CompiledStrategy, error)
	Evaluate(node RuleNode, ctx EvalContext) (bool, error)
}
