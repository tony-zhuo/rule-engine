package model

import (
	"fmt"
	"strings"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
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

// Duration converts the TimeWindow to a time.Duration.
func (w *TimeWindow) Duration() time.Duration {
	if w == nil {
		return 0
	}
	switch strings.ToLower(w.Unit) {
	case "minutes":
		return time.Duration(w.Value) * time.Minute
	case "hours":
		return time.Duration(w.Value) * time.Hour
	case "days":
		return time.Duration(w.Value) * 24 * time.Hour
	default:
		return time.Duration(w.Value) * time.Minute
	}
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

// CompiledRuleSet is the result of compiling all active strategies.
// It holds the compiled strategies, the pre-computed max window across
// all aggregate keys (for Redis event TTL/pruning), and per-behavior
// field schemas (for the zero-alloc pipe-separated event encoding).
type CompiledRuleSet struct {
	Strategies []CompiledStrategy
	MaxWindow  time.Duration
	Schemas    map[behaviorModel.BehaviorType]*behaviorModel.FieldSchema
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
