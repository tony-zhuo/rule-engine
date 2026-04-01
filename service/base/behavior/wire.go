package behavior

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	behaviorMySQL "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/mysql"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
)

var BehaviorRepoSet = wire.NewSet(
	behaviorMySQL.NewBehaviorRepo,
	wire.Bind(new(model.BehaviorRepoInterface), new(*behaviorMySQL.BehaviorRepo)),
)

var BehaviorUsecaseSet = wire.NewSet(
	usecase.NewBehaviorUsecase,
	wire.Bind(new(model.BehaviorUsecaseInterface), new(*usecase.BehaviorUsecase)),
)

var MockBehaviorProvider = wire.NewSet(BehaviorRepoSet, BehaviorUsecaseSet)
