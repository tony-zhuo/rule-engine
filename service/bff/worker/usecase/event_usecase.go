package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

// EventMessage is the Kafka message schema produced by the API's CheckEvent/ProcessEvent.
type EventMessage struct {
	EventID    string                     `json:"event_id"`
	MemberID   string                     `json:"member_id"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
}

// EventUsecase is the write-back worker that consumes events from Kafka
// and persists them to PostgreSQL for audit/reporting.
// Rule evaluation and CEP matching are now handled synchronously in the API.
type EventUsecase struct {
	behaviorRepo behaviorModel.BehaviorRepoInterface
}

func NewEventUsecase(
	behaviorRepo behaviorModel.BehaviorRepoInterface,
) *EventUsecase {
	return &EventUsecase{
		behaviorRepo: behaviorRepo,
	}
}

// Execute writes the event to PostgreSQL. Dedup is handled by the
// unique constraint on behavior_logs.event_id (ON CONFLICT DO NOTHING).
func (h *EventUsecase) Execute(msg *kafka.Message) error {
	ctx := context.Background()

	var event EventMessage
	if err := json.Unmarshal(msg.Value, &event); err != nil {
		return fmt.Errorf("unmarshal event: %w", err)
	}

	fieldsJSON, err := json.Marshal(event.Fields)
	if err != nil {
		return fmt.Errorf("marshal fields: %w", err)
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	if err := h.behaviorRepo.Create(ctx, &behaviorModel.BehaviorLog{
		EventID:    event.EventID,
		MemberID:   event.MemberID,
		Behavior:   event.Behavior,
		Fields:     string(fieldsJSON),
		OccurredAt: occurredAt,
	}); err != nil {
		slog.Warn("worker: write-back behavior log",
			"event_id", event.EventID, "error", err)
	}
	return nil
}
