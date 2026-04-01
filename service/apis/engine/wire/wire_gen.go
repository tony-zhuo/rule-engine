//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/service/apis/engine/controller"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

func InitializeRuleController(conf *initialize.Conf) *controller.RuleController {
	db := provideGormDB(conf)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC)
	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, behaviorUC)
	return controller.GetRuleController(engineUC)
}

func InitializeEventController(conf *initialize.Conf) *controller.EventController {
	db := provideGormDB(conf)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC)
	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, behaviorUC)
	return controller.GetEventController(engineUC)
}
