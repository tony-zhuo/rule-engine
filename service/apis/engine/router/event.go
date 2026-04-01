package router

import (
	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/wire"
)

func EventRegister(app *gin.RouterGroup) {
	ctrl := wire.InitializeEventController(initialize.GetConf())
	g := app.Group("/events")
	g.POST("", ctrl.ProcessEvent)
}
