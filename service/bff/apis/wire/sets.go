package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
)

var RuleAdminUsecaseSet = wire.NewSet(usecase.NewRuleAdminUsecase)
var RuleCtrlSet = wire.NewSet(controller.GetRuleController)
