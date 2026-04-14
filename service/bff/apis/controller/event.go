package controller

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/usecase"
)

var (
	_eventCtrlOnce sync.Once
	_eventCtrlObj  *EventController
)

type EventController struct {
	uc usecase.EngineUsecaseInterface
}

func GetEventController(uc usecase.EngineUsecaseInterface) *EventController {
	_eventCtrlOnce.Do(func() {
		_eventCtrlObj = &EventController{uc: uc}
	})
	return _eventCtrlObj
}

// ProcessEvent godoc
// @Summary Submit a behavioral event for async processing
// @Tags events
// @Accept json
// @Produce json
// @Param body body usecase.ProcessEventReq true "Event"
// @Success 202
// @Router /v1/events [post]
func (c *EventController) ProcessEvent(ctx *gin.Context) {
	var req usecase.ProcessEventReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := c.uc.ProcessEvent(ctx, &req); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusAccepted, gin.H{"status": "accepted"})
}

// CheckEvent godoc
// @Summary Evaluate rules and CEP patterns synchronously against an event
// @Tags events
// @Accept json
// @Produce json
// @Param body body usecase.CheckEventReq true "Event"
// @Success 200 {object} usecase.CheckEventResp
// @Router /v1/events/check [post]
func (c *EventController) CheckEvent(ctx *gin.Context) {
	var req usecase.CheckEventReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := c.uc.CheckEvent(ctx, &req)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, resp)
}
