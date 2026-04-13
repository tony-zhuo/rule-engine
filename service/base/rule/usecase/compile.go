package usecase

import (
	"fmt"
	"strings"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// Compile transforms a RuleNode AST into a CompiledRule closure tree.
// The AST is walked once at compile time; the returned function evaluates
// the rule without recursive dispatch or repeated string parsing.
func Compile(node model.RuleNode) (model.CompiledRule, error) {
	switch node.Type {
	case model.NodeAnd:
		return compileAnd(node.Children)
	case model.NodeOr:
		return compileOr(node.Children)
	case model.NodeNot:
		return compileNot(node.Children)
	case model.NodeCondition:
		return compileCondition(node)
	default:
		return nil, fmt.Errorf("compile: unknown node type %q", node.Type)
	}
}

func compileAnd(children []model.RuleNode) (model.CompiledRule, error) {
	compiled := make([]model.CompiledRule, len(children))
	for i, child := range children {
		c, err := Compile(child)
		if err != nil {
			return nil, err
		}
		compiled[i] = c
	}
	return func(ctx model.EvalContext) (bool, error) {
		for _, fn := range compiled {
			ok, err := fn(ctx)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		return true, nil
	}, nil
}

func compileOr(children []model.RuleNode) (model.CompiledRule, error) {
	compiled := make([]model.CompiledRule, len(children))
	for i, child := range children {
		c, err := Compile(child)
		if err != nil {
			return nil, err
		}
		compiled[i] = c
	}
	return func(ctx model.EvalContext) (bool, error) {
		for _, fn := range compiled {
			ok, err := fn(ctx)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	}, nil
}

func compileNot(children []model.RuleNode) (model.CompiledRule, error) {
	if len(children) != 1 {
		return nil, fmt.Errorf("compile: NOT node must have exactly one child, got %d", len(children))
	}
	inner, err := Compile(children[0])
	if err != nil {
		return nil, err
	}
	return func(ctx model.EvalContext) (bool, error) {
		ok, err := inner(ctx)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}, nil
}

func compileCondition(node model.RuleNode) (model.CompiledRule, error) {
	op := strings.ToUpper(node.Operator)
	field := node.Field
	window := node.Window
	value := node.Value

	// Pre-determine field resolution strategy.
	fieldIsVar := strings.HasPrefix(field, "$")
	fieldIsAggregate := !fieldIsVar && strings.Contains(field, ":")

	// Pre-determine RHS resolution strategy.
	rhsStr, rhsIsVarRef := value.(string)
	rhsIsVar := rhsIsVarRef && strings.HasPrefix(rhsStr, "$")

	// Build the resolver for the left-hand side.
	resolveLHS := buildLHSResolver(field, fieldIsVar, fieldIsAggregate, window)

	// Build the resolver for the right-hand side.
	resolveRHS := buildRHSResolver(value, rhsIsVar, rhsStr)

	// Build the comparator based on the operator (determined at compile time).
	cmp, err := buildComparator(op)
	if err != nil {
		return nil, err
	}

	return func(ctx model.EvalContext) (bool, error) {
		lhs, err := resolveLHS(ctx)
		if err != nil {
			return false, fmt.Errorf("evaluate condition field %q: %w", field, err)
		}
		rhs, err := resolveRHS(ctx)
		if err != nil {
			return false, fmt.Errorf("evaluate condition value: %w", err)
		}
		return cmp(lhs, rhs)
	}, nil
}

type valueResolver func(ctx model.EvalContext) (any, error)

func buildLHSResolver(field string, isVar, isAggregate bool, window *model.TimeWindow) valueResolver {
	switch {
	case isVar:
		varName := field[1:]
		return func(ctx model.EvalContext) (any, error) {
			v, _ := ctx.GetVariable(varName)
			return v, nil
		}
	case isAggregate:
		return func(ctx model.EvalContext) (any, error) {
			return ctx.Resolve(field, window)
		}
	default:
		return func(ctx model.EvalContext) (any, error) {
			return ctx.Resolve(field, nil)
		}
	}
}

func buildRHSResolver(value any, isVar bool, varRef string) valueResolver {
	if isVar {
		varName := varRef[1:]
		return func(ctx model.EvalContext) (any, error) {
			v, found := ctx.GetVariable(varName)
			if !found {
				return varRef, nil // return literal so comparison fails gracefully
			}
			return v, nil
		}
	}
	// Static value — captured at compile time.
	return func(_ model.EvalContext) (any, error) {
		return value, nil
	}
}

type comparator func(lhs, rhs any) (bool, error)

func buildComparator(op string) (comparator, error) {
	switch op {
	case "IN":
		return cmpIn, nil
	case "NOT_IN":
		return func(lhs, rhs any) (bool, error) {
			ok, err := cmpIn(lhs, rhs)
			return !ok, err
		}, nil
	case ">":
		return numericOrStringCmp(func(l, r float64) bool { return l > r }, nil), nil
	case "<":
		return numericOrStringCmp(func(l, r float64) bool { return l < r }, nil), nil
	case ">=":
		return numericOrStringCmp(func(l, r float64) bool { return l >= r }, nil), nil
	case "<=":
		return numericOrStringCmp(func(l, r float64) bool { return l <= r }, nil), nil
	case "=", "==":
		return numericOrStringCmp(
			func(l, r float64) bool { return l == r },
			func(l, r string) bool { return l == r },
		), nil
	case "!=":
		return numericOrStringCmp(
			func(l, r float64) bool { return l != r },
			func(l, r string) bool { return l != r },
		), nil
	default:
		return nil, fmt.Errorf("compile: unsupported operator %q", op)
	}
}

// numericOrStringCmp returns a comparator that tries numeric comparison first,
// then falls back to string comparison for equality operators.
func numericOrStringCmp(numFn func(float64, float64) bool, strFn func(string, string) bool) comparator {
	return func(lhs, rhs any) (bool, error) {
		lNum, lOk := toFloat64(lhs)
		rNum, rOk := toFloat64(rhs)
		if lOk && rOk {
			return numFn(lNum, rNum), nil
		}
		if strFn != nil {
			return strFn(fmt.Sprintf("%v", lhs), fmt.Sprintf("%v", rhs)), nil
		}
		return false, fmt.Errorf("operator requires numeric operands")
	}
}

func cmpIn(actual any, list any) (bool, error) {
	items, ok := list.([]any)
	if !ok {
		return false, fmt.Errorf("IN operator requires a []any value, got %T", list)
	}
	s := fmt.Sprintf("%v", actual)
	for _, item := range items {
		if s == fmt.Sprintf("%v", item) {
			return true, nil
		}
	}
	return false, nil
}
