package apis

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/router"
)

func ApiRegister(cfg *config.Config) {
	r := gin.New()
	r.Use(gin.Recovery())

	v1 := r.Group("/v1")
	router.RuleRegister(v1, cfg)
	router.EventRegister(v1, cfg)

	srv := &http.Server{
		Addr:    cfg.App.Addr,
		Handler: r,
	}
	srv.ListenAndServe()
}
