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

	inserted, err := store.StoreEvent(ctx, event, testMaxWindow)
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

	inserted1, _ := store.StoreEvent(ctx, event, testMaxWindow)
	inserted2, _ := store.StoreEvent(ctx, event, testMaxWindow)

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

	for i := range 5 {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": float64(100 * (i + 1))},
			OccurredAt: now.Add(-time.Duration(i) * time.Hour),
		}, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-24 * time.Hour), Key: "Trade:COUNT:|24hours"},
	})
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

	amounts := []float64{1000, 2000, 3000}
	for i, amt := range amounts {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorCryptoWithdraw,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Hour),
		}, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorCryptoWithdraw, Aggregation: "SUM", FieldPath: "amount",
			Since: now.Add(-24 * time.Hour), Key: "CryptoWithdraw:SUM:amount|24hours"},
	})
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

	for i, amt := range []float64{100, 200, 300} {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Minute),
		}, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "AVG", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "Trade:AVG:amount|1hours"},
	})
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

	for i, amt := range []float64{50, 300, 150} {
		store.StoreEvent(ctx, &model.BehaviorEvent{
			EventID:    "evt-" + string(rune('A'+i)),
			MemberID:   "user1",
			Behavior:   model.BehaviorTrade,
			Fields:     map[string]any{"amount": amt},
			OccurredAt: now.Add(-time.Duration(i) * time.Minute),
		}, testMaxWindow)
	}

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "MAX", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "max"},
		{Behavior: model.BehaviorTrade, Aggregation: "MIN", FieldPath: "amount",
			Since: now.Add(-1 * time.Hour), Key: "min"},
	})
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

	// 3 events: 30min ago, 2h ago, 25h ago
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "recent", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(100)}, OccurredAt: now.Add(-30 * time.Minute),
	}, testMaxWindow)
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "medium", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(200)}, OccurredAt: now.Add(-2 * time.Hour),
	}, testMaxWindow)
	store.StoreEvent(ctx, &model.BehaviorEvent{
		EventID: "old", MemberID: "user1", Behavior: model.BehaviorTrade,
		Fields: map[string]any{"amount": float64(500)}, OccurredAt: now.Add(-25 * time.Hour),
	}, testMaxWindow)

	results, err := store.BatchAggregate(ctx, "user1", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-1 * time.Hour), Key: "1h"},
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: now.Add(-24 * time.Hour), Key: "24h"},
	})
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

	results, err := store.BatchAggregate(ctx, "nonexistent", []model.AggregateCond{
		{Behavior: model.BehaviorTrade, Aggregation: "COUNT", Since: time.Now().Add(-24 * time.Hour), Key: "k"},
		{Behavior: model.BehaviorTrade, Aggregation: "SUM", FieldPath: "amount", Since: time.Now().Add(-24 * time.Hour), Key: "k2"},
	})
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

	results, err := store.BatchAggregate(ctx, "user1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %v", results)
	}
}
