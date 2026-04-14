//go:build !wireinject

package wire

import (
	"context"
	"log"
	"log/slog"

	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
	behaviorRedis "github.com/tony-zhuo/rule-engine/service/base/behavior/repository/redis"
	cepDB "github.com/tony-zhuo/rule-engine/service/base/cep/repository/db"
	cepRedis "github.com/tony-zhuo/rule-engine/service/base/cep/repository/redis"
	cepUsecase "github.com/tony-zhuo/rule-engine/service/base/cep/usecase"
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
	eventStore := behaviorRedis.NewBehaviorEventStore(rdb)
	cepStore := cepRedis.NewRedisStore(rdb)
	cepUC := cepUsecase.NewCEPUsecase(cepStore, ruleUC)
	loadCEPPatterns(cfg, cepUC)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, eventStore, cepUC, producer, cfg.Kafka.Topics.Events, cfg.Kafka.Topics.Results)
	return controller.GetRuleController(engineUC)
}

func InitializeEventController(cfg *config.Config) *controller.EventController {
	db := provideGormDB()
	rdb := provideRedisClient()
	producer := provideKafkaProducer(cfg)
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)
	eventStore := behaviorRedis.NewBehaviorEventStore(rdb)
	cepStore := cepRedis.NewRedisStore(rdb)
	cepUC := cepUsecase.NewCEPUsecase(cepStore, ruleUC)
	loadCEPPatterns(cfg, cepUC)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC, eventStore, cepUC, producer, cfg.Kafka.Topics.Events, cfg.Kafka.Topics.Results)
	return controller.GetEventController(engineUC)
}

func loadCEPPatterns(_ *config.Config, cepUC *cepUsecase.CEPUsecase) {
	db := provideGormDB()
	cepPatternRepo := cepDB.NewCEPPatternRepo(db)
	patterns, err := cepPatternRepo.ListActive(context.Background())
	if err != nil {
		log.Fatal("failed to load CEP patterns: ", err)
	}
	for _, p := range patterns {
		cepUC.AddPattern(p)
	}
	slog.Info("CEP patterns loaded (API)", "count", len(patterns))
}
