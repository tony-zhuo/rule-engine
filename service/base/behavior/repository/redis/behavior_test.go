package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
)

const testMaxWindow = 7 * 24 * time.Hour

// amountSchema is the test schema: single numeric field "amount" at position 0.
// Used for behaviors where rules aggregate only on amount.
var amountSchema = &model.FieldSchema{
	Fields: []string{"amount"},
	Index:  map[string]int{"amount": 0},
}

// schemasFor returns a map schema lookup for a single behavior.
func schemasFor(b model.BehaviorType, s *model.FieldSchema) map[model.BehaviorType]*model.FieldSchema {
	return map[model.BehaviorType]*model.FieldSchema{b: s}
}

func setupTestStore(t *testing.T) (*BehaviorEventStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	store := NewBehaviorEventStore(client)
	return store, mr
}

func TestStoreEvent_NewEvent(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	event := &model.BehaviorEvent{
		EventID:    "evt-001",
		MemberID:   "user1",
		Behavior:   model.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": float64(5000)},
		OccurredAt: time.Now(),
	}
	schemas := schemasFor(model.BehaviorCryptoWithdraw, amountSchema)

	inserted, err := store.StoreEvent(ctx, event, schemas, testMaxWindow)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Error("expected new event to be inserted")
	}
}

func TestStoreEvent_Dedup(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	event := &model.BehaviorEvent{
		EventID:    "evt-001",
		MemberID:   "user1",
		Behavior:   model.BehaviorCryptoWithdraw,
		Fields:     map[string]any{"amount": float64(5000)},
		OccurredAt: time.Now(),
	}
	schemas := schemasFor(model.BehaviorCryptoWithdraw, amountSchema)

	inserted1, _ := store.StoreEvent(ctx, event, schemas, testMaxWindow)
	inserted2, _ := store.StoreEvent(ctx, event, schemas, testMaxWindow)

	if !inserted1 {
		t.Error("first insert should succeed")
	}
	if inserted2 {
		t.Error("duplicate insert should be rejected")
	}
}

func TestBatchAggregate_COUNT(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	schemas := schemasFor(model.BehaviorTrade, amountSchema)

	for i := range 5 {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": float64(100 * (i + 1))},
			OccurredAt: now.Add(-time.Duration(i) * time.Hour),
		}, schemas, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-24 * time.Hour), Key: "Trade:COUNT:|24hours"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["Trade:COUNT:|24hours"] != 5 {
		t.Errorf("expected COUNT=5, got %v", results["Trade:COUNT:|24hours"])
	}
}

func TestBatchAggregate_SUM(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	schemas := schemasFor(model.BehaviorCryptoWithdraw, amountSchema)

	amounts := []float64{1000, 2000, 3000}
	for i, amt := range amounts {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorCryptoWithdraw,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Hour),
		}, schemas, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorCryptoWithdraw, Aggregation: "SUM", FieldPath: "amount",
			Since: now.Add(-24 * time.Hour), Key: "CryptoWithdraw:SUM:amount|24hours"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["CryptoWithdraw:SUM:amount|24hours"] != 6000 {
		t.Errorf("expected SUM=6000, got %v", results["CryptoWithdraw:SUM:amount|24hours"])
	}
}

func TestBatchAggregate_AVG(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	schemas := schemasFor(model.BehaviorTrade, amountSchema)

	for i, amt := range []float64{100, 200, 300} {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Minute),
		}, schemas, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "AVG", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "Trade:AVG:amount|1hours"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["Trade:AVG:amount|1hours"] != 200 {
		t.Errorf("expected AVG=200, got %v", results["Trade:AVG:amount|1hours"])
	}
}

func TestBatchAggregate_MAX_MIN(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	schemas := schemasFor(model.BehaviorTrade, amountSchema)

	for i, amt := range []float64{50, 300, 150} {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Minute),
		}, schemas, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "MAX", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "max"},
		{Behavior: model.BehaviorTrade, Aggregation: "MIN", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "min"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["max"] != 300 {
		t.Errorf("expected MAX=300, got %v", results["max"])
	}
	if results["min"] != 50 {
		t.Errorf("expected MIN=50, got %v", results["min"])
	}
}

func TestBatchAggregate_TimeWindowFilter(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()
	schemas := schemasFor(model.BehaviorTrade, amountSchema)

	// 3 events: 30min ago, 2h ago, 25h ago
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "recent", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(100)}, OccurredAt: now.Add(-30 * time.Minute),
	}, schemas, testMaxWindow)
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "medium", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(200)}, OccurredAt: now.Add(-2 * time.Hour),
	}, schemas, testMaxWindow)
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "old", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(500)}, OccurredAt: now.Add(-25 * time.Hour),
	}, schemas, testMaxWindow)

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-1 * time.Hour), Key: "1h"},
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-24 * time.Hour), Key: "24h"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["1h"] != 1 {
		t.Errorf("1h window: expected 1, got %v", results["1h"])
	}
	if results["24h"] != 2 {
		t.Errorf("24h window: expected 2, got %v", results["24h"])
	}
}

func TestBatchAggregate_EmptyData(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	schemas := schemasFor(model.BehaviorTrade, amountSchema)

	results, err := store.BatchAggregate(ctx, "nonexistent", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: time.Now().Add(-24 * time.Hour), Key: "k"},
		{Behavior: model.BehaviorTrade, Aggregation: "SUM", FieldPath: "amount", Since: time.Now().Add(-24 * time.Hour), Key: "k2"},
	}, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if results["k"] != 0 {
		t.Errorf("expected COUNT=0, got %v", results["k"])
	}
	if results["k2"] != 0 {
		t.Errorf("expected SUM=0, got %v", results["k2"])
	}
}

func TestBatchAggregate_NoConds(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	results, err := store.BatchAggregate(ctx, "user1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %v", results)
	}
}

// TestStoreAndAggregate_MultiField verifies that two numeric fields at different
// schema positions are encoded and decoded correctly in one pipeline round-trip.
func TestStoreAndAggregate_MultiField(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// Schema: amount at 0, fee at 1.
	schema := &model.FieldSchema{
		Fields: []string{"amount", "fee"},
		Index:  map[string]int{"amount": 0, "fee": 1},
	}
	schemas := schemasFor(model.BehaviorCryptoWithdraw, schema)

	// Seed 3 historical events for the same member.
	for i, pair := range []struct {
		amount, fee float64
	}{{1000, 5}, {2000, 10}, {3000, 15}} {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "seed-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorCryptoWithdraw,
			Fields:     map[string]any{"amount": pair.amount, "fee": pair.fee},
			OccurredAt: now.Add(-time.Duration(i+1) * time.Minute),
		}, schemas, testMaxWindow)
	}

	// Now the actual CheckEvent-style call: store a new event + aggregate.
	results, err := store.StoreAndAggregate(ctx,
		&model.BehaviorEvent{
			EventID:    "new",
			MemberID:   "user1",
			Behavior:   model.BehaviorCryptoWithdraw,
			Fields:     map[string]any{"amount": float64(4000), "fee": float64(20)},
			OccurredAt: now,
		},
		schemas,
		[]model.AggregateCond{
			{Behavior: model.BehaviorCryptoWithdraw, Aggregation: "SUM", FieldPath: "amount",
				Since: now.Add(-time.Hour), Key: "sum_amount"},
			{Behavior: model.BehaviorCryptoWithdraw, Aggregation: "MAX", FieldPath: "fee",
				Since: now.Add(-time.Hour), Key: "max_fee"},
		},
		testMaxWindow,
	)
	if err != nil {
		t.Fatal(err)
	}
	// Sum of amount = 1000 + 2000 + 3000 + 4000 = 10000
	if results["sum_amount"] != 10000 {
		t.Errorf("sum_amount: expected 10000, got %v", results["sum_amount"])
	}
	// Max fee = 20
	if results["max_fee"] != 20 {
		t.Errorf("max_fee: expected 20, got %v", results["max_fee"])
	}
}

// TestEncodeMember exercises the pipe-separated encoder directly.
func TestEncodeMember(t *testing.T) {
	schema := &model.FieldSchema{
		Fields: []string{"amount", "fee"},
		Index:  map[string]int{"amount": 0, "fee": 1},
	}
	event := &model.BehaviorEvent{
		EventID: "evt-1",
		Fields:  map[string]any{"amount": float64(1500), "fee": float64(10.5)},
	}
	got := encodeMember(event, schema)
	want := "evt-1|1500|10.5"
	if got != want {
		t.Errorf("encodeMember: got %q, want %q", got, want)
	}

	// Missing field → empty slot.
	event2 := &model.BehaviorEvent{
		EventID: "evt-2",
		Fields:  map[string]any{"amount": float64(100)},
	}
	got2 := encodeMember(event2, schema)
	want2 := "evt-2|100|"
	if got2 != want2 {
		t.Errorf("encodeMember with missing field: got %q, want %q", got2, want2)
	}

	// Nil schema → event_id only.
	got3 := encodeMember(&model.BehaviorEvent{EventID: "evt-3"}, nil)
	if got3 != "evt-3" {
		t.Errorf("encodeMember with nil schema: got %q, want %q", got3, "evt-3")
	}
}

// TestParseFieldAt exercises the zero-alloc field extractor directly.
func TestParseFieldAt(t *testing.T) {
	member := "evt-1|1500|10.5"
	if v, ok := parseFieldAt(member, 0); !ok || v != 1500 {
		t.Errorf("pos 0: got (%v, %v), want (1500, true)", v, ok)
	}
	if v, ok := parseFieldAt(member, 1); !ok || v != 10.5 {
		t.Errorf("pos 1: got (%v, %v), want (10.5, true)", v, ok)
	}
	if _, ok := parseFieldAt(member, 2); ok {
		t.Error("pos 2 out of range: expected ok=false")
	}
	if _, ok := parseFieldAt("evt-1|", 0); ok {
		t.Error("empty slot: expected ok=false")
	}
}
