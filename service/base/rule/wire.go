package rule

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/service/base/rule/model"
	"github.com/tony-zhuo/rule-engine/service/base/rule/usecase"
)

var RuleUsecaseSet = wire.NewSet(
	usecase.NewRuleUsecase,
	wire.Bind(new(model.RuleUsecaseInterface), new(*usecase.RuleUsecase)),
)

var MockRuleProvider = wire.NewSet(RuleUsecaseSet)
