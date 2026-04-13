package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

type EventMessage struct {
	EventID    string                     `json:"event_id"`
	MemberID   string                     `json:"member_id"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
}

type MatchResult struct {
	MemberID        string           `json:"member_id"`
	MatchedRules    []MatchedRule    `json:"matched_rules,omitempty"`
	MatchedPatterns []MatchedPattern `json:"matched_patterns,omitempty"`
	ProcessedAt     time.Time        `json:"processed_at"`
}

type MatchedRule struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type MatchedPattern struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Variables map[string]any `json:"variables,omitempty"`
}

const maxRetries = 3

type EventUsecase struct {
	behaviorUC       behaviorModel.BehaviorUsecaseInterface
	processedEventRepo behaviorModel.ProcessedEventRepoInterface
	ruleStrategyUC   ruleModel.RuleStrategyUsecaseInterface
	cepProcessor     cepModel.ProcessorInterface
	producer         *kafka.Producer
	resultsTopic     string
}

func NewEventUsecase(
	behaviorUC behaviorModel.BehaviorUsecaseInterface,
	processedEventRepo behaviorModel.ProcessedEventRepoInterface,
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	cepProcessor cepModel.ProcessorInterface,
	producer *kafka.Producer,
	resultsTopic string,
) *EventUsecase {
	return &EventUsecase{
		behaviorUC:         behaviorUC,
		processedEventRepo: processedEventRepo,
		ruleStrategyUC:     ruleStrategyUC,
		cepProcessor:       cepProcessor,
		producer:           producer,
		resultsTopic:       resultsTopic,
	}
}

func (h *EventUsecase) Execute(msg *kafka.Message) error {
	ctx := context.Background()

	var event EventMessage
	if err := json.Unmarshal(msg.Value, &event); err != nil {
		return fmt.Errorf("unmarshal event: %w", err)
	}

	// 1+2. Dedup + log behavior in single DB round-trip (CTE).
	fieldsJSON, err := json.Marshal(event.Fields)
	if err != nil {
		return fmt.Errorf("marshal fields: %w", err)
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	pe, err := h.processedEventRepo.UpsertWithBehaviorLog(ctx, event.EventID, &behaviorModel.BehaviorLog{
		EventID:    event.EventID,
		MemberID:   event.MemberID,
		Behavior:   event.Behavior,
		Fields:     string(fieldsJSON),
		OccurredAt: occurredAt,
	})
	if err != nil {
		return fmt.Errorf("upsert with behavior log: %w", err)
	}
	switch pe.Status {
	case behaviorModel.ProcessedEventStatusCompleted:
		slog.Debug("worker: skip completed event", "event_id", event.EventID)
		return nil
	case behaviorModel.ProcessedEventStatusFailed:
		slog.Debug("worker: skip failed event", "event_id", event.EventID)
		return nil
	default:
		if pe.Attempts > maxRetries {
			slog.Error("worker: event exceeded max retries",
				"event_id", event.EventID, "attempts", pe.Attempts)
			if err := h.processedEventRepo.MarkFailed(ctx, event.EventID); err != nil {
				slog.Error("worker: mark failed", "event_id", event.EventID, "error", err)
			}
			return nil
		}
	}

	// 3. List active compiled rules (from Redis cache, compiled on load).
	compiled, err := h.ruleStrategyUC.ListActiveCompiled(ctx)
	if err != nil {
		return fmt.Errorf("list active compiled rules: %w", err)
	}

	// 4. Batch aggregate — collect all unique keys from compiled strategies, single DB round-trip.
	var allKeys []ruleModel.AggregateKey
	seen := make(map[string]struct{})
	for _, cs := range compiled {
		for _, k := range cs.AggregateKeys {
			ck := k.CacheKey()
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				allKeys = append(allKeys, k)
			}
		}
	}

	now := time.Now()
	conds := buildAggregateConds(event.MemberID, allKeys, now)
	aggResults, err := h.behaviorUC.BatchAggregate(ctx, event.MemberID, conds)
	if err != nil {
		return fmt.Errorf("batch aggregate: %w", err)
	}
	cache := make(map[string]any, len(aggResults))
	for k, v := range aggResults {
		cache[k] = v
	}

	// 5. Evaluate compiled rules — no AST walking, direct closure execution.
	fields := make(map[string]any, len(event.Fields)+2)
	for k, v := range event.Fields {
		fields[k] = v
	}
	fields["behavior"] = string(event.Behavior)
	fields["member_id"] = event.MemberID

	var matched []MatchedRule
	for _, cs := range compiled {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, cache)
		ok, err := cs.Eval(evalCtx)
		if err != nil || !ok {
			continue
		}
		matched = append(matched, MatchedRule{ID: cs.ID, Name: cs.Name})
	}

	// 6. CEP pattern matching (idempotent via event_id dedup in progress).
	var matchedPatterns []MatchedPattern
	cepEvent := &cepModel.Event{
		EventID:    event.EventID,
		MemberID:   event.MemberID,
		Behavior:   string(event.Behavior),
		Fields:     event.Fields,
		OccurredAt: event.OccurredAt,
	}
	cepResults, err := h.cepProcessor.ProcessEvent(ctx, cepEvent)
	if err != nil {
		return fmt.Errorf("cep process event: %w", err)
	}
	for _, r := range cepResults {
		matchedPatterns = append(matchedPatterns, MatchedPattern{
			ID:        r.PatternID,
			Name:      r.PatternName,
			Variables: r.Variables,
		})
	}

	// 7. Produce match results (only if something matched).
	if len(matched) > 0 || len(matchedPatterns) > 0 {
		result := MatchResult{
			MemberID:        event.MemberID,
			MatchedRules:    matched,
			MatchedPatterns: matchedPatterns,
			ProcessedAt:     time.Now(),
		}
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		if err := pkgkafka.Produce(h.producer, h.resultsTopic, event.MemberID, data); err != nil {
			return fmt.Errorf("produce result: %w", err)
		}
	}

	// 8. Mark event as completed.
	if err := h.processedEventRepo.MarkCompleted(ctx, event.EventID); err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	return nil
}

func buildAggregateCond(memberID string, k ruleModel.AggregateKey, now time.Time) *behaviorModel.AggregateCond {
	parts := strings.SplitN(k.Field, ":", 3)
	fieldPath := ""
	if len(parts) == 3 {
		fieldPath = parts[2]
	}

	var since time.Time
	if k.Window != nil {
		switch strings.ToLower(k.Window.Unit) {
		case "minutes":
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		case "hours":
			since = now.Add(-time.Duration(k.Window.Value) * time.Hour)
		case "days":
			since = now.Add(-time.Duration(k.Window.Value) * 24 * time.Hour)
		default:
			since = now.Add(-time.Duration(k.Window.Value) * time.Minute)
		}
	}

	return &behaviorModel.AggregateCond{
		MemberID:    memberID,
		Behavior:    behaviorModel.BehaviorType(parts[0]),
		Aggregation: strings.ToUpper(parts[1]),
		FieldPath:   fieldPath,
		Since:       since,
		Key:         k.CacheKey(),
	}
}

func buildAggregateConds(memberID string, keys []ruleModel.AggregateKey, now time.Time) []behaviorModel.AggregateCond {
	conds := make([]behaviorModel.AggregateCond, 0, len(keys))
	for _, k := range keys {
		conds = append(conds, *buildAggregateCond(memberID, k, now))
	}
	return conds
}
