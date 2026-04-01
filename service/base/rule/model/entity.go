package model

type RuleNode struct {
	Type     NodeType   `json:"type"`
	Children []RuleNode `json:"children,omitempty"`
	Field    string     `json:"field,omitempty"`
	Operator string     `json:"operator,omitempty"`
	Value    any        `json:"value,omitempty"`
	Window   *TimeWindow `json:"window,omitempty"`
}

type TimeWindow struct {
	Value int    `json:"value"`
	Unit  string `json:"unit"` // "minutes", "hours", "days"
}
