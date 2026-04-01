//go:build wireinject
// +build wireinject

package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/controller"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	cepDomain "github.com/tony-zhuo/rule-engine/service/base/cep"
	ruleDomain "github.com/tony-zhuo/rule-engine/service/base/rule"
)

func InitializeEngineController(conf *initialize.Conf) *controller.EngineController {
	wire.Build(
		ConfigSet,
		ruleDomain.MockRuleProvider,
		cepDomain.MockCEPProvider,
		EngineUsecaseSet,
		EngineCtrlSet,
	)
	return &controller.EngineController{}
}
