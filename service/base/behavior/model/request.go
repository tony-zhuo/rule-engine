package model

import "time"

type LogBehaviorReq struct {
	EventID  string         `json:"event_id"`
	MemberID string         `json:"member_id"   binding:"required"`
	Behavior   BehaviorType   `json:"behavior"    binding:"required"`
	Fields     map[string]any `json:"fields"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type AggregateCond struct {
	MemberID    string
	Behavior    BehaviorType
	Aggregation string    // COUNT, SUM, AVG, MAX, MIN
	FieldPath   string    // JSON path e.g. "amount"; empty for COUNT
	Since       time.Time // occurred_at >= Since
}
