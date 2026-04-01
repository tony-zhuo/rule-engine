package model

type NodeType string

const (
	NodeAnd       NodeType = "AND"
	NodeOr        NodeType = "OR"
	NodeNot       NodeType = "NOT"
	NodeCondition NodeType = "CONDITION"
)
