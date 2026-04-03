package usecase

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
)

type EventMessage struct {
	MemberID   string                     `json:"member_id"`
	PlatformID string                     `json:"platform_id"`
	Behavior   behaviorModel.BehaviorType `json:"behavior"`
	Fields     map[string]any             `json:"fields"`
	OccurredAt time.Time                  `json:"occurred_at"`
}

type MatchResult struct {
	MemberID     string        `json:"member_id"`
	MatchedRules []MatchedRule `json:"matched_rules"`
	ProcessedAt  time.Time     `json:"processed_at"`
}

type MatchedRule struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type EventUsecase struct {
	behaviorUC     behaviorModel.BehaviorUsecaseInterface
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface
	producer       *kafka.Producer
	resultsTopic   string
}

func NewEventUsecase(
	behaviorUC behaviorModel.BehaviorUsecaseInterface,
	ruleStrategyUC ruleModel.RuleStrategyUsecaseInterface,
	producer *kafka.Producer,
	resultsTopic string,
) *EventUsecase {
	return &EventUsecase{
		behaviorUC:     behaviorUC,
		ruleStrategyUC: ruleStrategyUC,
		producer:       producer,
		resultsTopic:   resultsTopic,
	}
}

func (h *EventUsecase) Execute(msg *kafka.Message) {
	ctx := context.Background()

	var event EventMessage
	if err := json.Unmarshal(msg.Value, &event); err != nil {
		slog.Error("worker: unmarshal event", "error", err)
		return
	}

	// 1. Log behavior.
	if _, err := h.behaviorUC.Log(ctx, &behaviorModel.LogBehaviorReq{
		MemberID:   event.MemberID,
		PlatformID: event.PlatformID,
		Behavior:   event.Behavior,
		Fields:     event.Fields,
		OccurredAt: event.OccurredAt,
	}); err != nil {
		slog.Error("worker: log behavior", "error", err, "member_id", event.MemberID)
		return
	}

	// 2. List active rules (from Redis cache).
	rules, err := h.ruleStrategyUC.ListActive(ctx)
	if err != nil {
		slog.Error("worker: list active rules", "error", err)
		return
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
	var matched []MatchedRule
	for _, rule := range rules {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(event.Fields, cache)
		ok, err := h.ruleStrategyUC.Evaluate(rule.RuleNode, evalCtx)
		if err != nil || !ok {
			continue
		}
		matched = append(matched, MatchedRule{ID: rule.ID, Name: rule.Name})
	}

	if len(matched) == 0 {
		return
	}

	// 5. Produce match results.
	result := MatchResult{
		MemberID:     event.MemberID,
		MatchedRules: matched,
		ProcessedAt:  time.Now(),
	}
	data, err := json.Marshal(result)
	if err != nil {
		slog.Error("worker: marshal result", "error", err)
		return
	}
	if err := pkgkafka.Produce(h.producer, h.resultsTopic, event.MemberID, data); err != nil {
		slog.Error("worker: produce result", "error", err, "member_id", event.MemberID)
	}
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
