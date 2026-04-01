package model

// EvalContext provides field resolution and cross-event variable access.
type EvalContext interface {
	Resolve(field string, window *TimeWindow) (any, error)
	GetVariable(name string) (any, bool)
}

// RuleUsecaseInterface is the contract for the rule evaluation usecase.
type RuleUsecaseInterface interface {
	Evaluate(node RuleNode, ctx EvalContext) (bool, error)
}
