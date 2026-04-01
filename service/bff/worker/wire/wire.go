//go:build wireinject
// +build wireinject

package wire

import (
	"github.com/google/wire"
	"github.com/tony-zhuo/rule-engine/config"
	behaviorDomain "github.com/tony-zhuo/rule-engine/service/base/behavior"
	ruleDomain "github.com/tony-zhuo/rule-engine/service/base/rule"
	"github.com/tony-zhuo/rule-engine/service/bff/worker"
)

func InitializeHandler(cfg *config.Config) *worker.Handler {
	wire.Build(
		ConfigSet,
		ruleDomain.MockRuleProvider,
		behaviorDomain.MockBehaviorProvider,
		HandlerSet,
	)
	return &worker.Handler{}
}
