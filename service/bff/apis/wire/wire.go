//go:build wireinject
// +build wireinject

package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	cepDomain "github.com/tony-zhuo/rule-engine/service/base/cep"
	ruleDomain "github.com/tony-zhuo/rule-engine/service/base/rule"
)

func InitializeEngineController(cfg *config.Config) *controller.EngineController {
	wire.Build(
		ConfigSet,
		ruleDomain.MockRuleProvider,
		cepDomain.MockCEPProvider,
		EngineUsecaseSet,
		EngineCtrlSet,
	)
	return &controller.EngineController{}
}
