package usecase

import (
	"context"
	"sync"

	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

// EngineUsecaseInterface is the BFF contract for orchestrating rule + cep domains.
type EngineUsecaseInterface interface {
	EvaluateRule(node ruleModel.RuleNode, ctx ruleModel.EvalContext) (bool, error)
	ProcessEvent(ctx context.Context, event *cepModel.Event) ([]*cepModel.MatchResult, error)
	LoadPattern(p cepModel.CEPPattern)
}

var (
	_engineUCOnce sync.Once
	_engineUCObj  *EngineUsecase
)

var _ EngineUsecaseInterface = (*EngineUsecase)(nil)

type EngineUsecase struct {
	ruleUC ruleModel.RuleUsecaseInterface
	cepUC  cepModel.ProcessorInterface
}

func NewEngineUsecase(ruleUC ruleModel.RuleUsecaseInterface, cepUC cepModel.ProcessorInterface) *EngineUsecase {
	_engineUCOnce.Do(func() {
		_engineUCObj = &EngineUsecase{ruleUC: ruleUC, cepUC: cepUC}
	})
	return _engineUCObj
}

func (uc *EngineUsecase) EvaluateRule(node ruleModel.RuleNode, ctx ruleModel.EvalContext) (bool, error) {
	return uc.ruleUC.Evaluate(node, ctx)
}

func (uc *EngineUsecase) ProcessEvent(ctx context.Context, event *cepModel.Event) ([]*cepModel.MatchResult, error) {
	return uc.cepUC.ProcessEvent(ctx, event)
}

func (uc *EngineUsecase) LoadPattern(p cepModel.CEPPattern) {
	uc.cepUC.AddPattern(p)
}
