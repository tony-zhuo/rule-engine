package behavior

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
)

var BehaviorRepoSet = wire.NewSet(
	behaviorDB.NewBehaviorRepo,
	wire.Bind(new(model.BehaviorRepoInterface), new(*behaviorDB.BehaviorRepo)),
)

var BehaviorUsecaseSet = wire.NewSet(
	usecase.NewBehaviorUsecase,
	wire.Bind(new(model.BehaviorUsecaseInterface), new(*usecase.BehaviorUsecase)),
)

var MockBehaviorProvider = wire.NewSet(BehaviorRepoSet, BehaviorUsecaseSet)
