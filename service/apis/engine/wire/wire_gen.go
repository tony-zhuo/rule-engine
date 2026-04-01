//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/service/apis/engine/controller"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
	behaviorMySQL "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/mysql"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	ruleMySQL "github.com/tony-zhuo/rule-engine/service/base/rule/repository/mysql"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

func InitializeRuleController(conf *initialize.Conf) *controller.RuleController {
	db := provideGormDB(conf)
	ruleRepo := ruleMySQL.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC)
	behaviorRepo := behaviorMySQL.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, behaviorUC)
	return controller.GetRuleController(engineUC)
}

func InitializeEventController(conf *initialize.Conf) *controller.EventController {
	db := provideGormDB(conf)
	ruleRepo := ruleMySQL.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC)
	behaviorRepo := behaviorMySQL.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, behaviorUC)
	return controller.GetEventController(engineUC)
}
