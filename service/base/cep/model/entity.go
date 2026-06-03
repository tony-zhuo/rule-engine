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
	Name      string                `json:"name"`
	Condition ruleModel.RuleNode    `json:"condition"`
	MaxWait   *ruleModel.TimeWindow `json:"max_wait,omitempty"`
	// IsNegative inverts the state's meaning: instead of "this event must occur
	// to advance", it means "if any event matching Condition occurs within MaxWait,
	// abort the progress (the bad thing happened); otherwise emit a match when
	// the deadline passes (the bad thing did NOT happen)". Negative states must
	// be terminal — see AddPattern validation in cep.go. Mirrors Flink CEP's
	// `notFollowedBy(...).within(...)`.
	IsNegative     bool              `json:"is_negative,omitempty"`
	ContextBinding map[string]string `json:"context_binding,omitempty"`
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
	// NegativeDeadline is set when CurrentStep lands on a negative state. The
	// watermark-driven sweep (negative_queue.go) fires a match when the watermark
	// passes this deadline without an aborting event having arrived. Zero value
	// means "not currently in a negative state".
	NegativeDeadline time.Time `json:"negative_deadline,omitempty"`
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
