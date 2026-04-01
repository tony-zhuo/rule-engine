package router

import (
	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/wire"
)

func RuleRegister(app *gin.RouterGroup) {
	ctrl := wire.InitializeRuleController(initialize.GetConf())
	g := app.Group("/rules")
	g.GET("", ctrl.List)
	g.POST("", ctrl.Create)
	g.GET("/:id", ctrl.Get)
	g.PUT("/:id", ctrl.Update)
	g.PATCH("/:id/status", ctrl.SetStatus)
}
