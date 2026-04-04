package usecase

import (
	"context"
	"encoding/json"
	"fmt"
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

type EventUsecase struct {
	behaviorUC     behaviorModel.BehaviorUsecaseInterface
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
	cepProcessor   cepModel.ProcessorInterface
	producer       *kafka.Producer
	resultsTopic   string
}

func NewEventUsecase(
	behaviorUC behaviorModel.BehaviorUsecaseInterface,
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	cepProcessor cepModel.ProcessorInterface,
	producer *kafka.Producer,
	resultsTopic string,
) *EventUsecase {
	return &EventUsecase{
		behaviorUC:     behaviorUC,
		ruleStrategyUC: ruleStrategyUC,
		cepProcessor:   cepProcessor,
		producer:       producer,
		resultsTopic:   resultsTopic,
	}
}

func (h *EventUsecase) Execute(msg *kafka.Message) error {
	ctx := context.Background()

	var event EventMessage
	if err := json.Unmarshal(msg.Value, &event); err != nil {
		return fmt.Errorf("unmarshal event: %w", err)
	}

	// 1. Log behavior (idempotent via event_id unique constraint).
	if _, err := h.behaviorUC.Log(ctx, &behaviorModel.LogBehaviorReq{
		EventID:    event.EventID,
		MemberID:   event.MemberID,
		Behavior:   event.Behavior,
		Fields:     event.Fields,
		OccurredAt: event.OccurredAt,
	}); err != nil {
		return fmt.Errorf("log behavior: %w", err)
	}

	// 2. List active rules (from Redis cache).
	rules, err := h.ruleStrategyUC.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("list active rules: %w", err)
	}

	// 3. Preload aggregate conditions.
	var allKeys []ruleModel.AggregateKey
	seen := make(map[string]struct{})
	for _, rule := range rules {
		for _, k := range ruleModel.CollectAggregateKeys(rule.RuleNode) {
			ck := k.CacheKey()
			if _, ok := seen[ck]; !ok {
				seen[ck] = struct{}{}
				allKeys = append(allKeys, k)
			}
		}
	}

	cache := make(map[string]any, len(allKeys))
	now := time.Now()
	for _, k := range allKeys {
		cond := buildAggregateCond(event.MemberID, k, now)
		result, err := h.behaviorUC.Aggregate(ctx, cond)
		if err != nil {
			continue
		}
		cache[k.CacheKey()] = result
	}

	// 4. Evaluate rules.
	fields := make(map[string]any, len(event.Fields)+2)
	for k, v := range event.Fields {
		fields[k] = v
	}
	fields["behavior"] = string(event.Behavior)
	fields["member_id"] = event.MemberID

	var matched []MatchedRule
	for _, rule := range rules {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, cache)
		ok, err := h.ruleStrategyUC.Evaluate(rule.RuleNode, evalCtx)
		if err != nil || !ok {
			continue
		}
		matched = append(matched, MatchedRule{ID: rule.ID, Name: rule.Name})
	}

	// 5. CEP pattern matching.
	var matchedPatterns []MatchedPattern
	cepEvent := &cepModel.Event{
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

	if len(matched) == 0 && len(matchedPatterns) == 0 {
		return nil
	}

	// 6. Produce match results.
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
	}
}
