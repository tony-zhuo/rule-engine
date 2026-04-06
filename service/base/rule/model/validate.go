package model

import (
	"fmt"
	"regexp"
	"strings"
)

var allowedAggregations = map[string]struct{}{
	"COUNT": {},
	"SUM":   {},
	"AVG":   {},
	"MAX":   {},
	"MIN":   {},
}

var identifierRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidateAggregation checks that agg is an allowed SQL aggregate function.
func ValidateAggregation(agg string) error {
	if _, ok := allowedAggregations[strings.ToUpper(agg)]; !ok {
		return fmt.Errorf("invalid aggregation %q: must be one of COUNT, SUM, AVG, MAX, MIN", agg)
	}
	return nil
}

// ValidateFieldPath checks that fieldPath is a safe identifier (or empty for COUNT).
func ValidateFieldPath(fieldPath string) error {
	if fieldPath == "" {
		return nil
	}
	if !identifierRe.MatchString(fieldPath) {
		return fmt.Errorf("invalid field path %q: must match [a-zA-Z_][a-zA-Z0-9_]*", fieldPath)
	}
	return nil
}

// ValidateRuleNode recursively validates a RuleNode tree.
// For CONDITION nodes whose Field contains ":" (aggregate fields like "Trade:SUM:amount"),
// it validates that the aggregation and field path parts are safe.
func ValidateRuleNode(node RuleNode) error {
	if node.Type == NodeCondition {
		if strings.Contains(node.Field, ":") {
			parts := strings.SplitN(node.Field, ":", 3)
			if len(parts) < 2 {
				return fmt.Errorf("invalid aggregate field %q: expected at least Behavior:Aggregation", node.Field)
			}
			agg := parts[1]
			if err := ValidateAggregation(agg); err != nil {
				return fmt.Errorf("field %q: %w", node.Field, err)
			}
			if len(parts) == 3 {
				if err := ValidateFieldPath(parts[2]); err != nil {
					return fmt.Errorf("field %q: %w", node.Field, err)
				}
			}
		}
		return nil
	}
	for i := range node.Children {
		if err := ValidateRuleNode(node.Children[i]); err != nil {
			return err
		}
	}
	return nil
}
