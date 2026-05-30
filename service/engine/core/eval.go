package core

import (
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

// MatchedRule identifies a rule that fired for an event.
type MatchedRule struct {
	ID   uint64
	Name string
}

// evaluateRules runs every active compiled rule against the event, resolving
// aggregate fields from the member's in-memory buckets instead of Redis.
//
// The compiled rules, the EvalContext interface, and PreloadedEvalContext are
// reused unchanged from the existing engine — only the source of aggregate
// values changes (Redis StoreAndAggregate → computeBucketAggregation). This is
// the payoff of the AST engine being decoupled from storage.
//
// Window aggregations are anchored at event.OccurredAt (event time), not
// time.Now(), so results are identical whether processing live or replaying.
func evaluateRules(
	ms *MemberState,
	fields map[string]any,
	occurredAt time.Time,
	ruleSet *ruleModel.CompiledRuleSet,
) []MatchedRule {
	// Build the aggregate cache: one computed value per unique aggregate key,
	// keyed by AggregateKey.CacheKey() so PreloadedEvalContext can look it up.
	// (M2 optimization: precompute allKeys once per rule-set load instead of
	// per event — kept per-event here to mirror the existing hot path.)
	allKeys := ruleUsecase.CollectUniqueAggregateKeys(ruleSet.Strategies)
	aggCache := make(map[string]any, len(allKeys))
	for _, k := range allKeys {
		aggCache[k.CacheKey()] = computeBucketAggregation(ms, k, occurredAt)
	}

	var matched []MatchedRule
	for _, cs := range ruleSet.Strategies {
		evalCtx := ruleUsecase.NewPreloadedEvalContext(fields, aggCache)
		ok, err := cs.Eval(evalCtx)
		if err != nil || !ok {
			continue
		}
		matched = append(matched, MatchedRule{ID: cs.ID, Name: cs.Name})
	}
	return matched
}

// buildEvalFields flattens an event into the field map the rule engine expects:
// the raw fields plus "behavior" and "member_id" as addressable fields. Mirrors
// the existing engine's field construction so rule semantics are unchanged.
func buildEvalFields(event *behaviorModel.BehaviorEvent) map[string]any {
	fields := make(map[string]any, len(event.Fields)+2)
	for k, v := range event.Fields {
		fields[k] = v
	}
	fields["behavior"] = string(event.Behavior)
	fields["member_id"] = event.MemberID
	return fields
}
