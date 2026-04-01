//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

func InitializeRuleController(cfg *config.Config) *controller.RuleController {
	db := provideGormDB()
	rdb := provideRedisClient()
	producer := provideKafkaProducer(cfg)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, producer, cfg.Kafka.Topics.Events)
	return controller.GetRuleController(engineUC)
}

func InitializeEventController(cfg *config.Config) *controller.EventController {
	db := provideGormDB()
	rdb := provideRedisClient()
	producer := provideKafkaProducer(cfg)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, producer, cfg.Kafka.Topics.Events)
	return controller.GetEventController(engineUC)
}
