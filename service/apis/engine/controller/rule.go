package controller

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
	ruleModel "github.com/tony-zhuo/rule-engine/service/base/rule/model"
)

var (
	_ruleCtrlOnce sync.Once
	_ruleCtrlObj  *RuleController
)

type RuleController struct {
	uc usecase.EngineUsecaseInterface
}

func GetRuleController(uc usecase.EngineUsecaseInterface) *RuleController {
	_ruleCtrlOnce.Do(func() {
		_ruleCtrlObj = &RuleController{uc: uc}
	})
	return _ruleCtrlObj
}

// Create godoc
// @Summary Create a rule strategy
// @Tags rules
// @Accept json
// @Produce json
// @Param body body ruleModel.CreateRuleStrategyReq true "Request"
// @Success 200 {object} ruleModel.RuleStrategy
// @Router /v1/rules [post]
func (c *RuleController) Create(ctx *gin.Context) {
	var req ruleModel.CreateRuleStrategyReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rule, err := c.uc.CreateRule(ctx, &req)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"data": rule})
}

// Get godoc
// @Summary Get a rule strategy by ID
// @Tags rules
// @Param id path int true "Rule ID"
// @Success 200 {object} ruleModel.RuleStrategy
// @Router /v1/rules/{id} [get]
func (c *RuleController) Get(ctx *gin.Context) {
	id, err := parseID(ctx)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rule, err := c.uc.GetRule(ctx, id)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rule == nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"data": rule})
}

// List godoc
// @Summary List rule strategies
// @Tags rules
// @Param status query int false "Filter by status (1=active, 2=inactive)"
// @Success 200 {array} ruleModel.RuleStrategy
// @Router /v1/rules [get]
func (c *RuleController) List(ctx *gin.Context) {
	var status *ruleModel.RuleStrategyStatus
	if s := ctx.Query("status"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			return
		}
		sv := ruleModel.RuleStrategyStatus(v)
		status = &sv
	}
	rules, err := c.uc.ListRules(ctx, status)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"data": rules})
}

// Update godoc
// @Summary Update a rule strategy
// @Tags rules
// @Param id path int true "Rule ID"
// @Param body body ruleModel.UpdateRuleStrategyReq true "Request"
// @Success 200
// @Router /v1/rules/{id} [put]
func (c *RuleController) Update(ctx *gin.Context) {
	id, err := parseID(ctx)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var req ruleModel.UpdateRuleStrategyReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := c.uc.UpdateRule(ctx, id, &req); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{})
}

// SetStatus godoc
// @Summary Enable or disable a rule strategy
// @Tags rules
// @Param id path int true "Rule ID"
// @Param body body setStatusReq true "Request"
// @Success 200
// @Router /v1/rules/{id}/status [patch]
func (c *RuleController) SetStatus(ctx *gin.Context) {
	id, err := parseID(ctx)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var req setStatusReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := c.uc.SetRuleStatus(ctx, id, req.Status); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{})
}

type setStatusReq struct {
	Status ruleModel.RuleStrategyStatus `json:"status" binding:"required"`
}

func parseID(ctx *gin.Context) (uint64, error) {
	return strconv.ParseUint(ctx.Param("id"), 10, 64)
}
