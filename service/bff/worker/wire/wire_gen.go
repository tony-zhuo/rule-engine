//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/config"
	behaviorDB "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/db"
	behaviorUsecase "github.com/tony-zhuo/rule-engine/service/base/behavior/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
	"github.com/tony-zhuo/rule-engine/service/bff/worker"
)

func InitializeHandler(cfg *config.Config) *worker.Handler {
	db := provideGormDB()
	rdb := provideRedisClient()
	producer := provideKafkaProducer(cfg)
	behaviorRepo := behaviorDB.NewBehaviorRepo(db)
	behaviorUC := behaviorUsecase.NewBehaviorUsecase(behaviorRepo)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)
	return worker.NewHandler(behaviorUC, ruleStrategyUC, producer, cfg.Kafka.Topics.Results)
}
