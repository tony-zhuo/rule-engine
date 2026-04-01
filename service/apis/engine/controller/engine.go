package controller

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
	cepModel "github.com/tony-zhuo/rule-engine/service/base/cep/model"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_engineCtrlOnce sync.Once
	_engineCtrlObj  *EngineController
)

type EngineController struct {
	uc usecase.EngineUsecaseInterface
}

func GetEngineController(uc usecase.EngineUsecaseInterface) *EngineController {
	_engineCtrlOnce.Do(func() {
		_engineCtrlObj = &EngineController{uc: uc}
	})
	return _engineCtrlObj
}

// EvaluateRule godoc
// @Summary Evaluate a rule node against provided fields
// @Tags engine
// @Accept json
// @Produce json
// @Param body body EvaluateRuleReq true "Request"
// @Success 200 {object} EvaluateRuleResp
// @Router /v1/engine/evaluate [post]
func (c *EngineController) EvaluateRule(ctx *gin.Context) {
	var req EvaluateRuleReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	evalCtx := ruleModel.NewMapContext(req.Fields)
	matched, err := c.uc.EvaluateRule(req.Rule, evalCtx)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, EvaluateRuleResp{Matched: matched})
}

// ProcessEvent godoc
// @Summary Process a behavioral event through CEP patterns
// @Tags engine
// @Accept json
// @Produce json
// @Param body body cepModel.Event true "Event"
// @Success 200 {object} ProcessEventResp
// @Router /v1/engine/events [post]
func (c *EngineController) ProcessEvent(ctx *gin.Context) {
	var event cepModel.Event
	if err := ctx.ShouldBindJSON(&event); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	results, err := c.uc.ProcessEvent(ctx, &event)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, ProcessEventResp{Matches: results})
}

type EvaluateRuleReq struct {
	Rule   ruleModel.RuleNode `json:"rule"`
	Fields map[string]any     `json:"fields"`
}

type EvaluateRuleResp struct {
	Matched bool `json:"matched"`
}

type ProcessEventResp struct {
	Matches []*cepModel.MatchResult `json:"matches"`
}
