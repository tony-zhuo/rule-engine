package ast

// EvalContext provides field resolution and cross-event variable access during rule evaluation.
type EvalContext interface {
	// Resolve returns the value for a field, optionally scoped to a time window.
	// Implementations handle aggregation (counts, sums) when a window is present.
	Resolve(field string, window *TimeWindow) (any, error)

	// GetVariable returns a named variable injected by the CEP engine across event boundaries.
	GetVariable(name string) (any, bool)
}

// MapContext is a simple EvalContext backed by a flat map, suitable for unit tests and demos.
// Variables (injected by the CEP processor) are stored separately from event fields.
type MapContext struct {
	fields    map[string]any
	variables map[string]any
}

// NewMapContext creates a MapContext from an event-fields map.
func NewMapContext(fields map[string]any) *MapContext {
	return &MapContext{
		fields:    fields,
		variables: make(map[string]any),
	}
}

// WithVariables returns a copy of the context with additional CEP variables injected.
func (c *MapContext) WithVariables(vars map[string]any) *MapContext {
	merged := make(map[string]any, len(c.variables)+len(vars))
	for k, v := range c.variables {
		merged[k] = v
	}
	for k, v := range vars {
		merged[k] = v
	}
	return &MapContext{fields: c.fields, variables: merged}
}

// Resolve returns the field value from the underlying map.
// Window is ignored in MapContext — callers are expected to pre-aggregate values.
func (c *MapContext) Resolve(field string, _ *TimeWindow) (any, error) {
	v, ok := c.fields[field]
	if !ok {
		return nil, nil
	}
	return v, nil
}

// GetVariable returns a named variable previously injected into this context.
func (c *MapContext) GetVariable(name string) (any, bool) {
	v, ok := c.variables[name]
	return v, ok
}
