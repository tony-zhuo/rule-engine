package controller

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/usecase"
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
// @Summary Submit a behavioral event for rule evaluation
// @Tags events
// @Accept json
// @Produce json
// @Param body body usecase.ProcessEventReq true "Event"
// @Success 200 {object} usecase.ProcessEventResp
// @Router /v1/events [post]
func (c *EventController) ProcessEvent(ctx *gin.Context) {
	var req usecase.ProcessEventReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	resp, err := c.uc.ProcessEvent(ctx, &req)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"data": resp})
}
