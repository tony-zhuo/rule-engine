package model

import "time"

type BehaviorType string

const (
	BehaviorLogin          BehaviorType = "Login"
	BehaviorTrade          BehaviorType = "Trade"
	BehaviorCryptoWithdraw BehaviorType = "CryptoWithdraw"
	BehaviorCryptoDeposit  BehaviorType = "CryptoDeposit"
	BehaviorFiatWithdraw   BehaviorType = "FiatWithdraw"
	BehaviorFiatDeposit    BehaviorType = "FiatDeposit"
)

type BehaviorLog struct {
	ID         uint64       `gorm:"primaryKey;autoIncrement"`
	EventID  string       `gorm:"size:64;not null"`
	MemberID string       `gorm:"size:128;not null"`
	Behavior   BehaviorType `gorm:"size:64;not null"`
	Fields     string       `gorm:"type:jsonb;not null"` // raw JSON
	OccurredAt time.Time    `gorm:"not null"`
	CreatedAt  time.Time
}

func (BehaviorLog) TableName() string { return "behavior_logs" }

const (
	ProcessedEventStatusPending   = "pending"
	ProcessedEventStatusCompleted = "completed"
	ProcessedEventStatusFailed    = "failed"
)

type ProcessedEvent struct {
	EventID   string    `gorm:"primaryKey;size:64"`
	Attempts  int       `gorm:"not null;default:0"`
	Status    string    `gorm:"size:16;not null;default:pending"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (ProcessedEvent) TableName() string { return "processed_events" }

// BehaviorEvent is the in-memory representation of a behavioral event
// used by the Redis event store (hot path) before being written to PG asynchronously.
type BehaviorEvent struct {
	EventID    string         `json:"event_id"`
	MemberID   string         `json:"member_id"`
	Behavior   BehaviorType   `json:"behavior"`
	Fields     map[string]any `json:"fields"`
	OccurredAt time.Time      `json:"occurred_at"`
}
