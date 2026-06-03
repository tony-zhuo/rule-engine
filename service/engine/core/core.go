package core

import (
	"sync/atomic"
	"time"

	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// defaultMaxWindow bounds bucket retention when no rule declares a window.
const defaultMaxWindow = 24 * time.Hour

// defaultAllowedLateness is how far the watermark trails the max event time —
// the slack out-of-order events get before being treated as late.
const defaultAllowedLateness = 5 * time.Second

// Core is one shard's stateful event processor. All of its state lives in
// memory and is mutated only by the goroutine that calls ProcessEvent — the
// single-writer model that makes the lock-free state in state.go safe.
type Core struct {
	ShardID   int
	State     *ShardState
	ruleSet   *ruleModel.CompiledRuleSet
	maxWindow time.Duration

	watermark       atomic.Int64 // unix nanos; trails max event time by allowedLateness
	allowedLateness time.Duration
	lateSink        func(*behaviorModel.BehaviorEvent) // side output for late events (optional)

	patterns     []compiledPattern // registered CEP patterns
	negDeadlines negDeadlineHeap   // pending negative-pattern deadlines, ordered by deadline

	lastSeq   atomic.Uint64 // last NATS stream sequence applied (checkpointed with snapshot)
	replaying atomic.Bool   // true while rebuilding from the log — suppresses side effects
}

// BeginReplay / EndReplay bracket replay after a snapshot load. While replaying,
// state is rebuilt but external side effects (late sink, future result emission)
// are suppressed — re-emitting decisions for already-processed events is wrong.
func (c *Core) BeginReplay() { c.replaying.Store(true) }
func (c *Core) EndReplay()   { c.replaying.Store(false) }

// ProcessResult is the outcome of processing one event: the rules and CEP
// patterns that fired. Empty for late events (which are routed to the side output).
type ProcessResult struct {
	MatchedRules    []MatchedRule
	MatchedPatterns []*cepModel.MatchResult
}

// Option configures a Core at construction.
type Option func(*Core)

// WithAllowedLateness overrides the default out-of-order slack.
func WithAllowedLateness(d time.Duration) Option {
	return func(c *Core) { c.allowedLateness = d }
}

// WithLateSink registers a callback for events that arrive behind the watermark.
// This is the side-output channel for a batch/review backstop (plan gap #27);
// real-time decisions are not re-fired for late events.
func WithLateSink(fn func(*behaviorModel.BehaviorEvent)) Option {
	return func(c *Core) { c.lateSink = fn }
}

// NewCore builds a shard with an empty state and a fixed compiled rule set.
// (Hot-reload via atomic.Pointer is a later concern — see plan §Rule Lifecycle.)
func NewCore(shardID int, ruleSet *ruleModel.CompiledRuleSet, opts ...Option) *Core {
	maxWindow := ruleSet.MaxWindow
	if maxWindow == 0 {
		maxWindow = defaultMaxWindow
	}
	c := &Core{
		ShardID:         shardID,
		State:           NewShardState(),
		ruleSet:         ruleSet,
		maxWindow:       maxWindow,
		allowedLateness: defaultAllowedLateness,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ProcessEvent folds one event into the member's state and, if the event is on
// time, evaluates rules and advances CEP patterns. Returns what fired (empty for
// late events).
//
// Order matters: aggregations are updated *before* evaluation so the triggering
// event is itself counted in its window (the "5th failure fires the rule" case).
//
// This is the single entry point the NATS consumer (Task H) will call; keeping
// it free of any I/O is what lets the whole engine be tested with plain values.
func (c *Core) ProcessEvent(event *behaviorModel.BehaviorEvent) *ProcessResult {
	ms := c.State.getOrCreateMember(event.MemberID)
	late := c.isLate(event.OccurredAt)

	// Apply to aggregation regardless of lateness: bucket ops (count/sum/max/min)
	// are commutative, so state stays complete and order-independent. Idempotent
	// on event_id, so redelivery and replay are safe.
	numericFields := numericFieldsOf(event.Fields)
	updateAggregations(ms, event.Behavior, event.EventID, event.OccurredAt, numericFields, c.maxWindow)
	ms.LastSeenAt = laterOf(ms.LastSeenAt, event.OccurredAt)

	if late {
		// Too late for a real-time decision. Route to the side output for a
		// batch/review backstop; do not re-fire rules (compensation is gap #27).
		// Suppressed during replay — the side output already happened originally.
		if c.lateSink != nil && !c.replaying.Load() {
			c.lateSink(event)
		}
		return &ProcessResult{}
	}

	// On time: advance the watermark, then evaluate rules and CEP patterns.
	c.advanceWatermark(event.OccurredAt)
	fields := buildEvalFields(event)

	res := &ProcessResult{
		MatchedRules: evaluateRules(ms, fields, event.OccurredAt, c.ruleSet),
	}

	// CEP works on its own event view (sequence detection is order-sensitive, so
	// it runs only on the in-order, on-time path).
	cepEvent := &cepModel.Event{
		EventID:    event.EventID,
		MemberID:   event.MemberID,
		Behavior:   string(event.Behavior),
		Fields:     event.Fields,
		OccurredAt: event.OccurredAt,
	}
	watermark := c.Watermark()
	res.MatchedPatterns = c.processCEP(ms, cepEvent, watermark)

	// After the watermark moved forward, fire any negative-pattern deadlines
	// that the new watermark has passed. These can be for members other than the
	// current event's member, which is exactly the point — a member who's gone
	// silent still gets their negative match emitted as soon as time advances on
	// the shard.
	if expired := c.drainNegativeMatches(watermark); len(expired) > 0 {
		res.MatchedPatterns = append(res.MatchedPatterns, expired...)
	}

	return res
}
