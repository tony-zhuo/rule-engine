//go:build wireinject
// +build wireinject

package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	ruleDomain "github.com/tony-zhuo/rule-engine/service/base/rule"
)

// InitializeRuleController is the wire spec for the rule CRUD controller — the
// only controller in cmd/apis after CheckEvent was removed (Task M).
func InitializeRuleController(cfg *config.Config) *controller.RuleController {
	wire.Build(
		ConfigSet,
		ruleDomain.MockRuleProvider,
		EngineUsecaseSet,
		RuleCtrlSet,
	)
	return &controller.RuleController{}
}
