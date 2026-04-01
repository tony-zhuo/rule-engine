package model

type CreateRuleStrategyReq struct {
	Name        string   `json:"name"        binding:"required"`
	Description string   `json:"description"`
	RuleNode    RuleNode `json:"rule_node"   binding:"required"`
}

type UpdateRuleStrategyReq struct {
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	RuleNode    *RuleNode `json:"rule_node"`
}
