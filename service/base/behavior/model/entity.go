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

// BehaviorEvent is one behavioral event as it travels through the engine: the
// producer publishes it to the event log, the shard consumer decodes it, and
// ProcessEvent folds it into that member's in-memory state.
//
// EventID is assigned by the producer and must stay stable across redelivery
// and replay — every dedup point in the engine depends on it. See the plan's
// §Event Identity Contract for the full set of invariants.
type BehaviorEvent struct {
	EventID    string         `json:"event_id"`
	MemberID   string         `json:"member_id"`
	Behavior   BehaviorType   `json:"behavior"`
	Fields     map[string]any `json:"fields"`
	OccurredAt time.Time      `json:"occurred_at"`
}
