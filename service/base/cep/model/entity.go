package model

import (
	"time"

	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

type CEPPattern struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	States []PatternState `json:"states"`
}

type PatternState struct {
	Name           string                `json:"name"`
	Condition      ruleModel.RuleNode    `json:"condition"`
	MaxWait        *ruleModel.TimeWindow `json:"max_wait,omitempty"`
	ContextBinding map[string]string     `json:"context_binding,omitempty"`
}

type PatternProgress struct {
	ID              string         `json:"id"`
	PatternID       string         `json:"pattern_id"`
	MemberID        string         `json:"member_id"`
	CurrentStep     int            `json:"current_step"`
	Variables       map[string]any `json:"variables"`
	StartedAt       time.Time      `json:"started_at"`
	ExpiresAt       time.Time      `json:"expires_at"`
	ProcessedEvents []string       `json:"processed_events,omitempty"`
}

type Event struct {
	EventID    string         `json:"event_id"`
	MemberID   string         `json:"member_id"`
	Behavior   string         `json:"behavior"`
	Fields     map[string]any `json:"fields"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type MatchResult struct {
	PatternID   string
	PatternName string
	MemberID    string
	Variables   map[string]any
	MatchedAt   time.Time
}
