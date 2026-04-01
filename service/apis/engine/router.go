package engine

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/router"
)

func ApiRegister() {
	conf := initialize.GetConf()
	r := gin.New()
	r.Use(gin.Recovery())

	v1 := r.Group("/v1")
	router.RuleRegister(v1)
	router.EventRegister(v1)

	srv := &http.Server{
		Addr:    conf.App.Addr,
		Handler: r,
	}
	srv.ListenAndServe()
}
