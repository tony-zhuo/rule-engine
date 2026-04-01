package main

import (
	"context"
	"fmt"
	"time"

	cepMemory "github.com/tony-zhuo/rule-engine/service/base/cep/repository/memory"
	cepUsecase "github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	engineUsecase "github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
)

func main() {
	demo1AST()
	fmt.Println()
	demo2CEP()
}

// demo1AST demonstrates stateless AST rule evaluation.
//
// Rule: withdraw_count (3 days) > 5 AND (single_amount > 1,000,000 OR daily_total > 5,000,000)
func demo1AST() {
	fmt.Println("=== Demo 1: AST Rule Evaluation ===")

	rule := ruleModel.RuleNode{
		Type: ruleModel.NodeAnd,
		Children: []ruleModel.RuleNode{
			{
				Type:     ruleModel.NodeCondition,
				Field:    "withdraw_count_3d",
				Operator: ">",
				Value:    float64(5),
				Window:   &ruleModel.TimeWindow{Value: 3, Unit: "days"},
			},
			{
				Type: ruleModel.NodeOr,
				Children: []ruleModel.RuleNode{
					{
						Type:     ruleModel.NodeCondition,
						Field:    "single_amount",
						Operator: ">",
						Value:    float64(1_000_000),
					},
					{
						Type:     ruleModel.NodeCondition,
						Field:    "daily_total",
						Operator: ">",
						Value:    float64(5_000_000),
						Window:   &ruleModel.TimeWindow{Value: 1, Unit: "days"},
					},
				},
			},
		},
	}

	ruleUC := ruleUsecase.NewRuleUsecase()

	// Case 1: should trigger (count=8 > 5, single=1,500,000 > 1,000,000)
	ctx1 := ruleModel.NewMapContext(map[string]any{
		"withdraw_count_3d": float64(8),
		"single_amount":     float64(1_500_000),
		"daily_total":       float64(2_000_000),
	})
	result1, err := ruleUC.Evaluate(rule, ctx1)
	fmt.Printf("Case 1 (should match):    matched=%v  err=%v\n", result1, err)

	// Case 2: should NOT trigger (count=3 <= 5)
	ctx2 := ruleModel.NewMapContext(map[string]any{
		"withdraw_count_3d": float64(3),
		"single_amount":     float64(2_000_000),
		"daily_total":       float64(6_000_000),
	})
	result2, err := ruleUC.Evaluate(rule, ctx2)
	fmt.Printf("Case 2 (should not match): matched=%v  err=%v\n", result2, err)

	// Case 3: should trigger (count=6, daily_total=6,000,000)
	ctx3 := ruleModel.NewMapContext(map[string]any{
		"withdraw_count_3d": float64(6),
		"single_amount":     float64(500_000),
		"daily_total":       float64(6_000_000),
	})
	result3, err := ruleUC.Evaluate(rule, ctx3)
	fmt.Printf("Case 3 (should match):    matched=%v  err=%v\n", result3, err)
}

// demo2CEP demonstrates stateful CEP pattern matching using MemoryStore.
//
// Pattern: CryptoDeposit -> within 10 minutes CryptoWithdraw to the same address.
func demo2CEP() {
	fmt.Println("=== Demo 2: CEP Pattern Matching ===")

	store := cepMemory.NewMemoryStore()
	ruleUC := ruleUsecase.NewRuleUsecase()
	cepUC := cepUsecase.NewCEPUsecase(store, ruleUC)
	eng := engineUsecase.NewEngineUsecase(ruleUC, cepUC)

	depositThenWithdraw := cepModel.CEPPattern{
		ID:   "pattern-deposit-withdraw",
		Name: "Deposit then Withdraw to Same Address",
		States: []cepModel.PatternState{
			{
				Name: "deposit",
				Condition: ruleModel.RuleNode{
					Type:     ruleModel.NodeCondition,
					Field:    "behavior",
					Operator: "=",
					Value:    "CryptoDeposit",
				},
				// Capture the source address from the deposit event.
				ContextBinding: map[string]string{
					"deposit_source": "$event.source_address",
				},
			},
			{
				Name:    "withdraw",
				MaxWait: &ruleModel.TimeWindow{Value: 10, Unit: "minutes"},
				// Both conditions must hold:
				//   1. behavior = CryptoWithdraw
				//   2. target_address = the address captured from the deposit
				Condition: ruleModel.RuleNode{
					Type: ruleModel.NodeAnd,
					Children: []ruleModel.RuleNode{
						{
							Type:     ruleModel.NodeCondition,
							Field:    "behavior",
							Operator: "=",
							Value:    "CryptoWithdraw",
						},
						{
							// "$deposit_source" is resolved from the CEP variable store.
							Type:     ruleModel.NodeCondition,
							Field:    "target_address",
							Operator: "=",
							Value:    "$deposit_source", // cross-event variable reference
						},
					},
				},
			},
		},
	}

	eng.LoadPattern(depositThenWithdraw)

	ctx := context.Background()
	now := time.Now()
	memberID := "member-42"
	sharedAddress := "0xABCDEF1234567890"

	// Event 1: CryptoDeposit from sharedAddress.
	event1 := &cepModel.Event{
		MemberID:   memberID,
		PlatformID: "platform-1",
		Behavior:   "CryptoDeposit",
		Fields: map[string]any{
			"source_address": sharedAddress,
			"amount":         float64(50_000),
		},
		OccurredAt: now,
	}

	results1, err := eng.ProcessEvent(ctx, event1)
	fmt.Printf("After CryptoDeposit:  matches=%d  err=%v\n", len(results1), err)

	// Event 2: CryptoWithdraw to sharedAddress within 5 minutes.
	event2 := &cepModel.Event{
		MemberID:   memberID,
		PlatformID: "platform-1",
		Behavior:   "CryptoWithdraw",
		Fields: map[string]any{
			"target_address": sharedAddress,
			"amount":         float64(49_000),
		},
		OccurredAt: now.Add(5 * time.Minute),
	}

	results2, err := eng.ProcessEvent(ctx, event2)
	fmt.Printf("After CryptoWithdraw: matches=%d  err=%v\n", len(results2), err)
	for _, r := range results2 {
		fmt.Printf("  MATCH: pattern=%q  member=%s  vars=%v  at=%s\n",
			r.PatternName, r.MemberID, r.Variables, r.MatchedAt.Format(time.RFC3339))
	}

	// Event 3: Withdraw to a DIFFERENT address -- should not trigger.
	store2 := cepMemory.NewMemoryStore()
	ruleUC2 := ruleUsecase.NewRuleUsecase()
	cepUC2 := cepUsecase.NewCEPUsecase(store2, ruleUC2)
	eng2 := engineUsecase.NewEngineUsecase(ruleUC2, cepUC2)
	eng2.LoadPattern(depositThenWithdraw)

	_, _ = eng2.ProcessEvent(ctx, event1)
	event3 := &cepModel.Event{
		MemberID:   memberID,
		PlatformID: "platform-1",
		Behavior:   "CryptoWithdraw",
		Fields: map[string]any{
			"target_address": "0xDIFFERENT",
			"amount":         float64(49_000),
		},
		OccurredAt: now.Add(5 * time.Minute),
	}
	results3, err := eng2.ProcessEvent(ctx, event3)
	fmt.Printf("After Withdraw (wrong address): matches=%d  err=%v\n", len(results3), err)
}
