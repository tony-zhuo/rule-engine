package usecase

import (
	"strings"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// PreloadedEvalContext resolves aggregation fields from a pre-fetched cache
// instead of querying the DB on every Resolve call.
type PreloadedEvalContext struct {
	fields    map[string]any
	cache     map[string]any // key = AggregateKey.CacheKey(), value = aggregation result
	variables map[string]any
}

var _ model.EvalContext = (*PreloadedEvalContext)(nil)

func NewPreloadedEvalContext(fields map[string]any, cache map[string]any) *PreloadedEvalContext {
	return &PreloadedEvalContext{
		fields:    fields,
		cache:     cache,
		variables: make(map[string]any),
	}
}

func (c *PreloadedEvalContext) Resolve(field string, window *model.TimeWindow) (any, error) {
	if !strings.Contains(field, ":") {
		return c.fields[field], nil
	}
	key := model.AggregateKey{Field: field, Window: window}
	return c.cache[key.CacheKey()], nil
}

func (c *PreloadedEvalContext) GetVariable(name string) (any, bool) {
	v, ok := c.variables[name]
	return v, ok
}
