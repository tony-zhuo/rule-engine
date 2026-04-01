package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
)

var EngineUsecaseSet = wire.NewSet(usecase.NewEngineUsecase)
var RuleCtrlSet = wire.NewSet(controller.GetRuleController)
var EventCtrlSet = wire.NewSet(controller.GetEventController)
