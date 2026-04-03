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
	EventID  string       `gorm:"size:36;not null;default:''"`
	MemberID string       `gorm:"size:128;not null"`
	Behavior   BehaviorType `gorm:"size:64;not null"`
	Fields     string       `gorm:"type:jsonb;not null"` // raw JSON
	OccurredAt time.Time    `gorm:"not null"`
	CreatedAt  time.Time
}

func (BehaviorLog) TableName() string { return "behavior_logs" }
