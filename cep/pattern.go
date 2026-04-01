package cep

import (
	"time"

	"github.com/tony-zhuo/rule-engine/ast"
)

// CEPPattern describes a multi-step event sequence to detect.
type CEPPattern struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	States []PatternState `json:"states"`
}

// PatternState is one step in a CEP pattern.
// The first state (index 0) starts a new pattern instance when its condition matches.
// Subsequent states must match within MaxWait of the previous step.
type PatternState struct {
	Name    string         `json:"name"`
	// Condition is evaluated against the incoming event.
	// Conditions may reference variables bound by earlier states using "$var_name" as the field.
	Condition      ast.RuleNode      `json:"condition"`
	MaxWait        *ast.TimeWindow   `json:"max_wait,omitempty"`
	// ContextBinding maps variable_name -> "$event.field_path".
	// When this state is matched, the listed fields are extracted from the event and stored
	// in PatternProgress.Variables for use in subsequent state conditions.
	ContextBinding map[string]string `json:"context_binding,omitempty"`
}

// Event is an inbound behavioral event from a member.
type Event struct {
	MemberID   string         `json:"member_id"`
	PlatformID string         `json:"platform_id"`
	Behavior   string         `json:"behavior"`   // e.g. CryptoWithdraw, Login
	Fields     map[string]any `json:"fields"`      // all event data
	OccurredAt time.Time      `json:"occurred_at"`
}

// MatchResult is emitted when a CEP pattern has been fully matched.
type MatchResult struct {
	PatternID   string
	PatternName string
	MemberID    string
	Variables   map[string]any // all accumulated variables across states
	MatchedAt   time.Time
}
