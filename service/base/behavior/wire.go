package behavior

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/model"
	behaviorPostgres "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/postgres"
	"github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
)

var BehaviorRepoSet = wire.NewSet(
	behaviorPostgres.NewBehaviorRepo,
	wire.Bind(new(model.BehaviorRepoInterface), new(*behaviorPostgres.BehaviorRepo)),
)

var BehaviorUsecaseSet = wire.NewSet(
	usecase.NewBehaviorUsecase,
	wire.Bind(new(model.BehaviorUsecaseInterface), new(*usecase.BehaviorUsecase)),
)

var MockBehaviorProvider = wire.NewSet(BehaviorRepoSet, BehaviorUsecaseSet)
