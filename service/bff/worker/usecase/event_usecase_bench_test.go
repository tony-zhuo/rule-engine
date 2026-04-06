package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	behaviorModel "github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	cepDB "github.com/tony-zhuo/rule-engine/service/base/cep/repository/db"
	cepRedis "github.com/tony-zhuo/rule-engine/service/base/cep/repository/redis"
	cepUsecase "github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
)

// setupIntegration initializes real DB, Redis, Kafka connections.
// Requires running infrastructure (docker compose up).
func setupIntegration(tb testing.TB) *EventUsecase {
	tb.Helper()

	pkgdb.Init(pkgdb.DBConfig{
		Host: "localhost", User: "rule_engine", Password: "rule_engine",
		DBName: "rule_engine", Port: 5432, SSLMode: "disable", TimeZone: "UTC",
	})
	pkgredis.Init(pkgredis.RedisConfig{MasterName: "mymaster", SentinelAddrs: "localhost:26379"})

	db := pkgdb.GetDB()
	rdb := pkgredis.GetClient()

	if db == nil {
		tb.Skip("DB not available, skipping integration benchmark")
	}
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		tb.Skip("Redis not available, skipping integration benchmark")
	}

	producer, err := pkgkafka.NewProducer(pkgkafka.KafkaConfig{Brokers: "localhost:9092"})
	if err != nil {
		tb.Skip("Kafka not available, skipping integration benchmark")
	}

	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)

	// CEP: load patterns from DB, use Redis for progress state.
	cepPatternRepo := cepDB.NewCEPPatternRepo(db)
	cepStore := cepRedis.NewRedisStore(rdb)
	cepUC := cepUsecase.NewCEPUsecase(cepStore, ruleUC)

	patterns, err := cepPatternRepo.ListActive(context.Background())
	if err != nil {
		tb.Fatalf("failed to load CEP patterns: %v", err)
	}
	for _, p := range patterns {
		cepUC.AddPattern(p)
	}

	return NewEventUsecase(behaviorUC, ruleStrategyUC, cepUC, producer, "rule-engine-results")
}

func buildMessage(eventID, memberID string, behavior behaviorModel.BehaviorType, fields map[string]any) *kafka.Message {
	msg := EventMessage{
		EventID:    eventID,
		MemberID:   memberID,
		Behavior:   behavior,
		Fields:     fields,
		OccurredAt: time.Now(),
	}
	data, _ := json.Marshal(msg)
	topic := "rule-engine-events"
	return &kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic},
		Value:          data,
	}
}

// BenchmarkExecute_Integration measures end-to-end Execute latency
// including DB write, Redis cache, rule evaluation, and Kafka produce.
//
// Run: go test -bench=BenchmarkExecute_Integration -benchtime=10s -count=3 ./service/bff/worker/usecase/
func BenchmarkExecute_Integration(b *testing.B) {
	handler := setupIntegration(b)
	prefix := rand.Int63()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg := buildMessage(
			fmt.Sprintf("bench-%d-%d", prefix, i),
			fmt.Sprintf("bench-member-%d", i%100),
			behaviorModel.BehaviorCryptoWithdraw,
			map[string]any{"amount": 150000, "target_address": "0xBENCH"},
		)
		if err := handler.Execute(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExecute_Integration_NoMatch benchmarks events that don't match any rule.
func BenchmarkExecute_Integration_NoMatch(b *testing.B) {
	handler := setupIntegration(b)
	prefix := rand.Int63()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg := buildMessage(
			fmt.Sprintf("bench-nomatch-%d-%d", prefix, i),
			fmt.Sprintf("bench-member-%d", i%100),
			behaviorModel.BehaviorTrade,
			map[string]any{"amount": 100, "pair": "BTC/USDT"},
		)
		if err := handler.Execute(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExecute_Integration_HighAggregation benchmarks events with many aggregate queries.
func BenchmarkExecute_Integration_HighAggregation(b *testing.B) {
	handler := setupIntegration(b)
	prefix := rand.Int63()

	// Pre-seed some behavior logs for aggregation queries to have data.
	for i := 0; i < 50; i++ {
		msg := buildMessage(
			fmt.Sprintf("bench-seed-%d-%d", prefix, i),
			"bench-member-agg",
			behaviorModel.BehaviorCryptoWithdraw,
			map[string]any{"amount": 10000, "target_address": "0xSEED"},
		)
		handler.Execute(msg)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg := buildMessage(
			fmt.Sprintf("bench-agg-%d-%d", prefix, i),
			"bench-member-agg",
			behaviorModel.BehaviorCryptoWithdraw,
			map[string]any{"amount": 5000, "target_address": "0xAGG"},
		)
		if err := handler.Execute(msg); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Unit benchmarks (no external deps) ---

type mockCEPProcessor struct{}

func (m *mockCEPProcessor) AddPattern(_ cepModel.CEPPattern) {}
func (m *mockCEPProcessor) ProcessEvent(_ context.Context, _ *cepModel.Event) ([]*cepModel.MatchResult, error) {
	return nil, nil
}

type mockBehaviorUC struct{}

func (m *mockBehaviorUC) Log(_ context.Context, req *behaviorModel.LogBehaviorReq) (*behaviorModel.BehaviorLog, error) {
	return &behaviorModel.BehaviorLog{ID: 1, EventID: req.EventID, MemberID: req.MemberID}, nil
}

func (m *mockBehaviorUC) Aggregate(_ context.Context, _ *behaviorModel.AggregateCond) (float64, error) {
	return 5, nil
}

type mockRuleStrategyUC struct {
	rules []*ruleModel.RuleStrategy
}

func (m *mockRuleStrategyUC) Get(_ context.Context, _ uint64) (*ruleModel.RuleStrategy, error) {
	return nil, nil
}
func (m *mockRuleStrategyUC) List(_ context.Context, _ *ruleModel.RuleStrategyStatus) ([]*ruleModel.RuleStrategy, error) {
	return m.rules, nil
}
func (m *mockRuleStrategyUC) Create(_ context.Context, _ *ruleModel.CreateRuleStrategyReq) (*ruleModel.RuleStrategy, error) {
	return nil, nil
}
func (m *mockRuleStrategyUC) Update(_ context.Context, _ uint64, _ *ruleModel.UpdateRuleStrategyReq) error {
	return nil
}
func (m *mockRuleStrategyUC) SetStatus(_ context.Context, _ uint64, _ ruleModel.RuleStrategyStatus) error {
	return nil
}
func (m *mockRuleStrategyUC) ListActive(_ context.Context) ([]*ruleModel.RuleStrategy, error) {
	return m.rules, nil
}
func (m *mockRuleStrategyUC) Evaluate(node ruleModel.RuleNode, ctx ruleModel.EvalContext) (bool, error) {
	return ruleUsecase.Evaluate(node, ctx)
}

func buildMockRules() []*ruleModel.RuleStrategy {
	return []*ruleModel.RuleStrategy{
		{
			ID: 1, Name: "單筆大額提幣",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeAnd,
				Children: []ruleModel.RuleNode{
					{Type: ruleModel.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
					{Type: ruleModel.NodeCondition, Field: "amount", Operator: ">", Value: float64(100000)},
				},
			},
		},
		{
			ID: 2, Name: "高頻交易",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeCondition, Field: "Trade:COUNT", Operator: ">", Value: float64(50),
				Window: &ruleModel.TimeWindow{Value: 3, Unit: "days"},
			},
		},
		{
			ID: 3, Name: "大額入金後快速提幣",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeAnd,
				Children: []ruleModel.RuleNode{
					{Type: ruleModel.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
					{Type: ruleModel.NodeCondition, Field: "FiatDeposit:COUNT", Operator: ">", Value: float64(0),
						Window: &ruleModel.TimeWindow{Value: 1, Unit: "hours"}},
					{Type: ruleModel.NodeCondition, Field: "FiatDeposit:MAX:amount", Operator: ">", Value: float64(50000),
						Window: &ruleModel.TimeWindow{Value: 1, Unit: "hours"}},
				},
			},
		},
		{
			ID: 4, Name: "異常登入後提幣",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeAnd,
				Children: []ruleModel.RuleNode{
					{Type: ruleModel.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
					{Type: ruleModel.NodeCondition, Field: "Login:COUNT", Operator: ">", Value: float64(0),
						Window: &ruleModel.TimeWindow{Value: 30, Unit: "minutes"}},
					{Type: ruleModel.NodeCondition, Field: "country", Operator: "NOT_IN", Value: []any{"TW", "JP", "US"}},
				},
			},
		},
		{
			ID: 5, Name: "分散小額提幣",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeAnd,
				Children: []ruleModel.RuleNode{
					{Type: ruleModel.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
					{Type: ruleModel.NodeCondition, Field: "amount", Operator: "<", Value: float64(10000)},
					{Type: ruleModel.NodeCondition, Field: "CryptoWithdraw:COUNT", Operator: ">", Value: float64(10),
						Window: &ruleModel.TimeWindow{Value: 24, Unit: "hours"}},
				},
			},
		},
		{
			ID: 6, Name: "累積大額提幣",
			RuleNode: ruleModel.RuleNode{
				Type: ruleModel.NodeCondition, Field: "CryptoWithdraw:SUM:amount", Operator: ">", Value: float64(500000),
				Window: &ruleModel.TimeWindow{Value: 7, Unit: "days"},
			},
		},
	}
}

// BenchmarkExecute_Unit measures Execute latency with mocked dependencies.
// This isolates the rule evaluation logic from I/O.
//
// Run: go test -bench=BenchmarkExecute_Unit -benchtime=10s -count=3 ./service/bff/worker/usecase/
func BenchmarkExecute_Unit(b *testing.B) {
	handler := &EventUsecase{
		behaviorUC:     &mockBehaviorUC{},
		ruleStrategyUC: &mockRuleStrategyUC{rules: buildMockRules()},
		cepProcessor:   &mockCEPProcessor{},
		producer:       nil,
		resultsTopic:   "test-results",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg := buildMessage(
			fmt.Sprintf("unit-%d", i),
			"member-unit",
			behaviorModel.BehaviorCryptoWithdraw,
			map[string]any{"amount": 150000, "target_address": "0xUNIT"},
		)
		handler.Execute(msg)
	}
}

// BenchmarkExecute_Unit_ManyRules measures impact of rule count on evaluation time.
func BenchmarkExecute_Unit_ManyRules(b *testing.B) {
	baseRules := buildMockRules()
	// Duplicate rules to simulate 50 active rules.
	var manyRules []*ruleModel.RuleStrategy
	for i := 0; i < 50; i++ {
		for _, r := range baseRules {
			copy := *r
			copy.ID = uint64(i*len(baseRules)) + r.ID
			manyRules = append(manyRules, &copy)
		}
	}

	handler := &EventUsecase{
		behaviorUC:     &mockBehaviorUC{},
		ruleStrategyUC: &mockRuleStrategyUC{rules: manyRules},
		cepProcessor:   &mockCEPProcessor{},
		producer:       nil,
		resultsTopic:   "test-results",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg := buildMessage(
			fmt.Sprintf("unit-many-%d", i),
			"member-unit",
			behaviorModel.BehaviorCryptoWithdraw,
			map[string]any{"amount": 150000, "target_address": "0xUNIT"},
		)
		handler.Execute(msg)
	}
}
