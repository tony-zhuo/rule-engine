package model

import "time"

type LogBehaviorReq struct {
	MemberID   string         `json:"member_id"   binding:"required"`
	PlatformID string         `json:"platform_id"`
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
