package rule

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
	rulePostgres "github.com/tony-zhuo/rule-engine/service/base/rule/repository/postgres"
	"github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

var RuleUsecaseSet = wire.NewSet(
	usecase.NewRuleUsecase,
	wire.Bind(new(model.RuleUsecaseInterface), new(*usecase.RuleUsecase)),
)

var RuleStrategyRepoSet = wire.NewSet(
	rulePostgres.NewRuleStrategyRepo,
	wire.Bind(new(model.RuleStrategyRepoInterface), new(*rulePostgres.RuleStrategyRepo)),
)

var RuleStrategyUsecaseSet = wire.NewSet(
	usecase.NewRuleStrategyUsecase,
	wire.Bind(new(model.RuleStrategyUsecaseInterface), new(*usecase.RuleStrategyUsecase)),
)

var MockRuleProvider = wire.NewSet(
	RuleUsecaseSet,
	RuleStrategyRepoSet,
	RuleStrategyUsecaseSet,
)
