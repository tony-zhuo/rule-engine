package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// DBEvalContext implements model.EvalContext with DB-backed aggregation support.
// Fields without ":" are resolved from the current event's fields map.
// Fields with ":" use the convention "Behavior:Aggregation" or "Behavior:Aggregation:FieldPath"
// and query behavior_logs via the behavior usecase.
type DBEvalContext struct {
	memberID   string
	fields     map[string]any
	variables  map[string]any
	behaviorUC behaviorModel.BehaviorUsecaseInterface
	now        time.Time
}

var _ model.EvalContext = (*DBEvalContext)(nil)

func NewDBEvalContext(
	memberID string,
	fields map[string]any,
	behaviorUC behaviorModel.BehaviorUsecaseInterface,
) *DBEvalContext {
	return &DBEvalContext{
		memberID:   memberID,
		fields:     fields,
		variables:  make(map[string]any),
		behaviorUC: behaviorUC,
		now:        time.Now(),
	}
}

func (c *DBEvalContext) WithVariables(vars map[string]any) *DBEvalContext {
	merged := make(map[string]any, len(c.variables)+len(vars))
	for k, v := range c.variables {
		merged[k] = v
	}
	for k, v := range vars {
		merged[k] = v
	}
	return &DBEvalContext{
		memberID:   c.memberID,
		fields:     c.fields,
		variables:  merged,
		behaviorUC: c.behaviorUC,
		now:        c.now,
	}
}

func (c *DBEvalContext) GetVariable(name string) (any, bool) {
	v, ok := c.variables[name]
	return v, ok
}

// Resolve resolves a field value.
// Fields containing ":" are resolved via DB aggregation query.
// All others are looked up from the current event's fields map.
func (c *DBEvalContext) Resolve(field string, window *model.TimeWindow) (any, error) {
	if !strings.Contains(field, ":") {
		return c.fields[field], nil
	}
	return c.resolveDB(context.Background(), field, window)
}

// resolveDB parses "Behavior:Aggregation[:FieldPath]" and executes the aggregation query.
func (c *DBEvalContext) resolveDB(ctx context.Context, field string, window *model.TimeWindow) (any, error) {
	parts := strings.SplitN(field, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("db_context: invalid field %q, expected Behavior:Aggregation[:FieldPath]", field)
	}

	fieldPath := ""
	if len(parts) == 3 {
		fieldPath = parts[2]
	}

	var since time.Time
	if window != nil {
		since = c.windowStart(window)
	}

	result, err := c.behaviorUC.Aggregate(ctx, &behaviorModel.AggregateCond{
		MemberID:    c.memberID,
		Behavior:    behaviorModel.BehaviorType(parts[0]),
		Aggregation: strings.ToUpper(parts[1]),
		FieldPath:   fieldPath,
		Since:       since,
	})
	if err != nil {
		return nil, fmt.Errorf("db_context aggregate %q: %w", field, err)
	}
	return result, nil
}

func (c *DBEvalContext) windowStart(w *model.TimeWindow) time.Time {
	switch strings.ToLower(w.Unit) {
	case "minutes":
		return c.now.Add(-time.Duration(w.Value) * time.Minute)
	case "hours":
		return c.now.Add(-time.Duration(w.Value) * time.Hour)
	case "days":
		return c.now.Add(-time.Duration(w.Value) * 24 * time.Hour)
	default:
		return c.now.Add(-time.Duration(w.Value) * time.Minute)
	}
}
