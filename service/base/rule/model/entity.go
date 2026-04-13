package model

import (
	"fmt"
	"strings"
	"time"
)

// CompiledRule is a pre-compiled evaluator produced from a RuleNode AST.
// It captures the tree structure in closures so evaluation avoids recursive
// tree walking, repeated operator parsing, and per-call type detection.
type CompiledRule func(ctx EvalContext) (bool, error)

type RuleNode struct {
	Type     NodeType    `json:"type"`
	Children []RuleNode  `json:"children,omitempty"`
	Field    string      `json:"field,omitempty"`
	Operator string      `json:"operator,omitempty"`
	Value    any         `json:"value,omitempty"`
	Window   *TimeWindow `json:"window,omitempty"`
}

type TimeWindow struct {
	Value int    `json:"value"`
	Unit  string `json:"unit"` // "minutes", "hours", "days"
}

type RuleStrategyStatus int

const (
	RuleStrategyStatusActive   RuleStrategyStatus = 1
	RuleStrategyStatusInactive RuleStrategyStatus = 2
)

type RuleStrategy struct {
	ID           uint64             `gorm:"primaryKey;autoIncrement"`
	Name         string             `gorm:"size:255;not null"`
	Description  string             `gorm:"type:text"`
	RuleNode     RuleNode           `gorm:"-"`
	RuleNodeJSON string             `gorm:"column:rule_node;type:jsonb;not null"`
	Status       RuleStrategyStatus `gorm:"not null;default:1"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (RuleStrategy) TableName() string { return "rule_strategies" }

// CompiledStrategy wraps a RuleStrategy with its pre-compiled evaluator and
// pre-extracted aggregate keys, eliminating per-event AST walking and key collection.
type CompiledStrategy struct {
	ID            uint64
	Name          string
	Eval          CompiledRule
	AggregateKeys []AggregateKey
}

// AggregateKey represents a unique aggregation query needed during rule evaluation.
type AggregateKey struct {
	Field  string
	Window *TimeWindow
}

// CacheKey returns a unique string key for deduplication.
func (k AggregateKey) CacheKey() string {
	if k.Window == nil {
		return k.Field
	}
	return fmt.Sprintf("%s|%d%s", k.Field, k.Window.Value, k.Window.Unit)
}

// CollectAggregateKeys walks a RuleNode tree and returns all aggregation fields (containing ":").
func CollectAggregateKeys(node RuleNode) []AggregateKey {
	seen := make(map[string]struct{})
	var keys []AggregateKey
	collectKeys(node, seen, &keys)
	return keys
}

func collectKeys(node RuleNode, seen map[string]struct{}, keys *[]AggregateKey) {
	if node.Type == NodeCondition {
		if strings.Contains(node.Field, ":") {
			k := AggregateKey{Field: node.Field, Window: node.Window}
			ck := k.CacheKey()
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				*keys = append(*keys, k)
			}
		}
		return
	}
	for _, child := range node.Children {
		collectKeys(child, seen, keys)
	}
}
