package router

import (
	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/wire"
)

func EngineRegister(app *gin.RouterGroup) {
	conf := initialize.GetConf()
	ctrl := wire.InitializeEngineController(conf)

	g := app.Group("/engine")
	g.POST("/evaluate", ctrl.EvaluateRule)
	g.POST("/events", ctrl.ProcessEvent)
}
