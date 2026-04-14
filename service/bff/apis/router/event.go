package router

import (
	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/wire"
)

func EventRegister(app *gin.RouterGroup, cfg *config.Config) {
	ctrl := wire.InitializeEventController(cfg)
	g := app.Group("/events")
	g.POST("", ctrl.ProcessEvent)
	g.POST("/check", ctrl.CheckEvent)
}
