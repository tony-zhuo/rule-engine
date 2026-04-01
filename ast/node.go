package ast

// NodeType identifies what kind of node a RuleNode is.
type NodeType string

const (
	NodeAnd       NodeType = "AND"
	NodeOr        NodeType = "OR"
	NodeNot       NodeType = "NOT"
	NodeCondition NodeType = "CONDITION"
)

// RuleNode is a single node in the rule AST.
// AND/OR/NOT nodes use Children; CONDITION nodes use Field/Operator/Value/Window.
type RuleNode struct {
	Type     NodeType   `json:"type"`
	Children []RuleNode `json:"children,omitempty"`
	Field    string     `json:"field,omitempty"`
	Operator string     `json:"operator,omitempty"` // >, <, >=, <=, =, !=, IN, NOT_IN
	Value    any        `json:"value,omitempty"`
	Window   *TimeWindow `json:"window,omitempty"`
}

// TimeWindow describes a rolling time range used in aggregation conditions.
type TimeWindow struct {
	Value int    `json:"value"`
	Unit  string `json:"unit"` // "minutes", "hours", "days"
}
