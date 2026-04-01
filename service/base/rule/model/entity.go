package model

import "time"

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
