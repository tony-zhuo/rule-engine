package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/controller"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
)

var EngineUsecaseSet = wire.NewSet(usecase.NewEngineUsecase)
var EngineCtrlSet = wire.NewSet(controller.GetEngineController)
