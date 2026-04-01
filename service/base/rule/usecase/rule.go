package usecase

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// RuleUsecase implements model.RuleUsecaseInterface.
type RuleUsecase struct{}

var (
	_ruleUsecaseOnce sync.Once
	_ruleUsecaseObj  *RuleUsecase
)

var _ model.RuleUsecaseInterface = (*RuleUsecase)(nil)

func NewRuleUsecase() *RuleUsecase {
	_ruleUsecaseOnce.Do(func() {
		_ruleUsecaseObj = &RuleUsecase{}
	})
	return _ruleUsecaseObj
}

func (uc *RuleUsecase) Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	return Evaluate(node, ctx)
}

// Evaluate recursively walks the RuleNode tree and returns whether the rule matches the context.
func Evaluate(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	switch node.Type {
	case model.NodeAnd:
		return evalAnd(node.Children, ctx)
	case model.NodeOr:
		return evalOr(node.Children, ctx)
	case model.NodeNot:
		return evalNot(node.Children, ctx)
	case model.NodeCondition:
		return evalCondition(node, ctx)
	default:
		return false, fmt.Errorf("evaluate: unknown node type %q", node.Type)
	}
}

func evalAnd(children []model.RuleNode, ctx model.EvalContext) (bool, error) {
	for _, child := range children {
		ok, err := Evaluate(child, ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalOr(children []model.RuleNode, ctx model.EvalContext) (bool, error) {
	for _, child := range children {
		ok, err := Evaluate(child, ctx)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func evalNot(children []model.RuleNode, ctx model.EvalContext) (bool, error) {
	if len(children) != 1 {
		return false, fmt.Errorf("evaluate: NOT node must have exactly one child, got %d", len(children))
	}
	ok, err := Evaluate(children[0], ctx)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

func evalCondition(node model.RuleNode, ctx model.EvalContext) (bool, error) {
	actual, err := resolveValue(node.Field, node.Window, ctx)
	if err != nil {
		return false, fmt.Errorf("evaluate condition field %q: %w", node.Field, err)
	}

	// The Value field may itself be a "$variable" reference (common in CEP cross-event conditions).
	expected, err := resolveRHS(node.Value, ctx)
	if err != nil {
		return false, fmt.Errorf("evaluate condition value: %w", err)
	}

	op := strings.ToUpper(node.Operator)

	switch op {
	case "IN":
		return evalIn(actual, expected)
	case "NOT_IN":
		ok, err := evalIn(actual, expected)
		return !ok, err
	}

	// For ordered comparisons we need numeric or string values.
	// Try numeric path first, fall back to string equality.
	lhsNum, lhsIsNum := toFloat64(actual)
	rhsNum, rhsIsNum := toFloat64(expected)

	if lhsIsNum && rhsIsNum {
		return compareNumbers(op, lhsNum, rhsNum)
	}

	// String / equality path.
	lhsStr := fmt.Sprintf("%v", actual)
	rhsStr := fmt.Sprintf("%v", expected)
	return compareStrings(op, lhsStr, rhsStr)
}

// resolveRHS resolves the right-hand side value of a condition.
// If value is a string starting with "$", it is treated as a variable reference.
func resolveRHS(value any, ctx model.EvalContext) (any, error) {
	s, ok := value.(string)
	if !ok || !strings.HasPrefix(s, "$") {
		return value, nil
	}
	varName := s[1:]
	v, found := ctx.GetVariable(varName)
	if !found {
		// Variable not yet bound; return the literal string so comparisons fail gracefully.
		return value, nil
	}
	return v, nil
}

// resolveValue checks variable store first ($var_name syntax), then falls back to field resolution.
func resolveValue(field string, window *model.TimeWindow, ctx model.EvalContext) (any, error) {
	if strings.HasPrefix(field, "$") {
		varName := field[1:]
		v, ok := ctx.GetVariable(varName)
		if !ok {
			return nil, nil
		}
		return v, nil
	}
	return ctx.Resolve(field, window)
}

func compareNumbers(op string, lhs, rhs float64) (bool, error) {
	switch op {
	case ">":
		return lhs > rhs, nil
	case "<":
		return lhs < rhs, nil
	case ">=":
		return lhs >= rhs, nil
	case "<=":
		return lhs <= rhs, nil
	case "=", "==":
		return lhs == rhs, nil
	case "!=":
		return lhs != rhs, nil
	default:
		return false, fmt.Errorf("evaluate: unsupported operator %q for numeric comparison", op)
	}
}

func compareStrings(op string, lhs, rhs string) (bool, error) {
	switch op {
	case "=", "==":
		return lhs == rhs, nil
	case "!=":
		return lhs != rhs, nil
	default:
		return false, fmt.Errorf("evaluate: operator %q not supported for string comparison", op)
	}
}

func evalIn(actual any, list any) (bool, error) {
	items, ok := list.([]any)
	if !ok {
		return false, fmt.Errorf("evaluate: IN operator requires a []any value, got %T", list)
	}
	for _, item := range items {
		if fmt.Sprintf("%v", actual) == fmt.Sprintf("%v", item) {
			return true, nil
		}
	}
	return false, nil
}

// toFloat64 coerces numeric types to float64. Returns (0, false) for non-numeric values.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}
