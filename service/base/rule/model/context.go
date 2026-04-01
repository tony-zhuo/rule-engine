package model

// MapContext is a simple EvalContext backed by a flat map.
type MapContext struct {
	fields    map[string]any
	variables map[string]any
}

func NewMapContext(fields map[string]any) *MapContext {
	return &MapContext{
		fields:    fields,
		variables: make(map[string]any),
	}
}

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

func (c *MapContext) Resolve(field string, _ *TimeWindow) (any, error) {
	v, ok := c.fields[field]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (c *MapContext) GetVariable(name string) (any, bool) {
	v, ok := c.variables[name]
	return v, ok
}
