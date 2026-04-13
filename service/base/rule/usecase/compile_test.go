package usecase

import (
	"testing"

	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// simpleCtx implements EvalContext for testing.
type simpleCtx struct {
	fields    map[string]any
	variables map[string]any
}

func (c *simpleCtx) Resolve(field string, _ *model.TimeWindow) (any, error) {
	return c.fields[field], nil
}

func (c *simpleCtx) GetVariable(name string) (any, bool) {
	v, ok := c.variables[name]
	return v, ok
}

func newCtx(fields map[string]any) *simpleCtx {
	return &simpleCtx{fields: fields, variables: make(map[string]any)}
}

func newCtxWithVars(fields map[string]any, vars map[string]any) *simpleCtx {
	return &simpleCtx{fields: fields, variables: vars}
}

func TestCompile_SimpleCondition(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "amount", Operator: ">", Value: float64(100),
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		amount float64
		want   bool
	}{
		{"above threshold", 200, true},
		{"at threshold", 100, false},
		{"below threshold", 50, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fn(newCtx(map[string]any{"amount": tt.amount}))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompile_AND(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeAnd,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
			{Type: model.NodeCondition, Field: "amount", Operator: ">", Value: float64(1000)},
		},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{"both match", map[string]any{"behavior": "CryptoWithdraw", "amount": float64(2000)}, true},
		{"behavior mismatch", map[string]any{"behavior": "Trade", "amount": float64(2000)}, false},
		{"amount mismatch", map[string]any{"behavior": "CryptoWithdraw", "amount": float64(500)}, false},
		{"both mismatch", map[string]any{"behavior": "Trade", "amount": float64(500)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fn(newCtx(tt.fields))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompile_OR(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeOr,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "amount", Operator: ">", Value: float64(10000)},
			{Type: model.NodeCondition, Field: "risk_score", Operator: ">=", Value: float64(90)},
		},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{"first matches", map[string]any{"amount": float64(20000), "risk_score": float64(10)}, true},
		{"second matches", map[string]any{"amount": float64(100), "risk_score": float64(95)}, true},
		{"both match", map[string]any{"amount": float64(20000), "risk_score": float64(95)}, true},
		{"none match", map[string]any{"amount": float64(100), "risk_score": float64(10)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fn(newCtx(tt.fields))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompile_NOT(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeNot,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "country", Operator: "=", Value: "TW"},
		},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fn(newCtx(map[string]any{"country": "TW"}))
	if err != nil {
		t.Fatal(err)
	}
	if got != false {
		t.Error("expected false for NOT(country=TW) when country is TW")
	}

	got, err = fn(newCtx(map[string]any{"country": "US"}))
	if err != nil {
		t.Fatal(err)
	}
	if got != true {
		t.Error("expected true for NOT(country=TW) when country is US")
	}
}

func TestCompile_Nested(t *testing.T) {
	// AND(behavior=CryptoWithdraw, OR(amount>10000, NOT(country=TW)))
	node := model.RuleNode{
		Type: model.NodeAnd,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
			{
				Type: model.NodeOr,
				Children: []model.RuleNode{
					{Type: model.NodeCondition, Field: "amount", Operator: ">", Value: float64(10000)},
					{
						Type: model.NodeNot,
						Children: []model.RuleNode{
							{Type: model.NodeCondition, Field: "country", Operator: "=", Value: "TW"},
						},
					},
				},
			},
		},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{"withdraw + high amount + TW", map[string]any{"behavior": "CryptoWithdraw", "amount": float64(20000), "country": "TW"}, true},
		{"withdraw + low amount + non-TW", map[string]any{"behavior": "CryptoWithdraw", "amount": float64(100), "country": "US"}, true},
		{"withdraw + low amount + TW", map[string]any{"behavior": "CryptoWithdraw", "amount": float64(100), "country": "TW"}, false},
		{"non-withdraw", map[string]any{"behavior": "Trade", "amount": float64(20000), "country": "US"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fn(newCtx(tt.fields))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompile_IN(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "country", Operator: "IN",
		Value: []any{"US", "UK", "JP"},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		country string
		want    bool
	}{
		{"US", true},
		{"JP", true},
		{"TW", false},
	}
	for _, tt := range tests {
		t.Run(tt.country, func(t *testing.T) {
			got, err := fn(newCtx(map[string]any{"country": tt.country}))
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompile_NOT_IN(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "country", Operator: "NOT_IN",
		Value: []any{"CN", "RU"},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := fn(newCtx(map[string]any{"country": "TW"}))
	if !got {
		t.Error("TW should not be in blocklist")
	}
	got, _ = fn(newCtx(map[string]any{"country": "CN"}))
	if got {
		t.Error("CN should be in blocklist")
	}
}

func TestCompile_VariableBinding(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "$saved_amount", Operator: ">", Value: float64(500),
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fn(newCtxWithVars(nil, map[string]any{"saved_amount": float64(1000)}))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for $saved_amount=1000 > 500")
	}
}

func TestCompile_RHSVariable(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "amount", Operator: "=", Value: "$prev_amount",
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fn(newCtxWithVars(
		map[string]any{"amount": float64(999)},
		map[string]any{"prev_amount": float64(999)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true when amount equals prev_amount")
	}
}

func TestCompile_StringEquality(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "status", Operator: "!=", Value: "blocked",
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := fn(newCtx(map[string]any{"status": "active"}))
	if !got {
		t.Error("active != blocked should be true")
	}
	got, _ = fn(newCtx(map[string]any{"status": "blocked"}))
	if got {
		t.Error("blocked != blocked should be false")
	}
}

func TestCompile_AggregateField(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeCondition, Field: "CryptoWithdraw:SUM:amount", Operator: ">", Value: float64(50000),
		Window: &model.TimeWindow{Value: 24, Unit: "hours"},
	}
	fn, err := Compile(node)
	if err != nil {
		t.Fatal(err)
	}

	ctx := NewPreloadedEvalContext(
		map[string]any{},
		map[string]any{"CryptoWithdraw:SUM:amount|24hours": float64(60000)},
	)
	got, err := fn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("expected true for aggregate sum 60000 > 50000")
	}
}

func TestCompile_ErrorOnUnknownNodeType(t *testing.T) {
	node := model.RuleNode{Type: "INVALID"}
	_, err := Compile(node)
	if err == nil {
		t.Error("expected error for unknown node type")
	}
}

func TestCompile_ErrorOnNOTMultipleChildren(t *testing.T) {
	node := model.RuleNode{
		Type: model.NodeNot,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "a", Operator: "=", Value: "1"},
			{Type: model.NodeCondition, Field: "b", Operator: "=", Value: "2"},
		},
	}
	_, err := Compile(node)
	if err == nil {
		t.Error("expected error for NOT with multiple children")
	}
}

// BenchmarkCompile_vs_Interpret compares compiled evaluation against interpreted.
func BenchmarkCompile_vs_Interpret(b *testing.B) {
	node := model.RuleNode{
		Type: model.NodeAnd,
		Children: []model.RuleNode{
			{Type: model.NodeCondition, Field: "behavior", Operator: "=", Value: "CryptoWithdraw"},
			{Type: model.NodeCondition, Field: "amount", Operator: ">", Value: float64(1000)},
			{
				Type: model.NodeOr,
				Children: []model.RuleNode{
					{Type: model.NodeCondition, Field: "risk_score", Operator: ">=", Value: float64(80)},
					{Type: model.NodeCondition, Field: "country", Operator: "IN", Value: []any{"CN", "RU", "IR"}},
				},
			},
		},
	}

	ctx := newCtx(map[string]any{
		"behavior":   "CryptoWithdraw",
		"amount":     float64(5000),
		"risk_score": float64(85),
		"country":    "US",
	})

	b.Run("interpreted", func(b *testing.B) {
		for b.Loop() {
			Evaluate(node, ctx)
		}
	})

	fn, err := Compile(node)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("compiled", func(b *testing.B) {
		for b.Loop() {
			fn(ctx)
		}
	})
}
