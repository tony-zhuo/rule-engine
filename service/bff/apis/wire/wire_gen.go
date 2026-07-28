//go:build !wireinject

package wire

import (
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/controller"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
	ruleDB "github.com/tony-zhuo/rule-engine/service/base/rule/repository/db"
	ruleUsecase "github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

// InitializeRuleController wires the rule CRUD controller — the only controller
// in cmd/apis, talking only to PostgreSQL.
func InitializeRuleController(_ *config.Config) *controller.RuleController {
	db := provideGormDB()
	ruleRepo := ruleDB.NewRuleStrategyRepo(db)
	ruleUC := ruleUsecase.NewRuleUsecase()
	ruleStrategyUC := ruleUsecase.NewRuleStrategyUsecase(ruleRepo, ruleUC)
	ruleAdminUC := usecase.NewRuleAdminUsecase(ruleStrategyUC)
	return controller.GetRuleController(ruleAdminUC)
}
