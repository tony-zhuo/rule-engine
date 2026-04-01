package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/controller"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
)

var EngineUsecaseSet = wire.NewSet(usecase.NewEngineUsecase)
var RuleCtrlSet = wire.NewSet(controller.GetRuleController)
var EventCtrlSet = wire.NewSet(controller.GetEventController)
