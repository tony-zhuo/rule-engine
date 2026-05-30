//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

// InitializeRuleController wires the rule CRUD controller. After Task M removed
// CheckEvent, this is the only controller in cmd/apis — the behavior store, CEP
// processor, and Kafka producer that the sync path used are no longer wired.
func InitializeRuleController(_ *config.Config) *controller.RuleController {
	db := provideGormDB()
	rdb := provideRedisClient()
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC, rdb)
	engineUC := usecase.NewEngineUsecase(ruleStrategyUC)
	return controller.GetRuleController(engineUC)
}
