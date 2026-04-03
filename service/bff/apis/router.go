package apis

import (
	"context"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/router"
	"github.com/tony-zhuo/rule-engine/service/bff/apis/wire"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
		}
	}()

	slog.Info("api server started", "addr", cfg.App.Addr)

	<-ctx.Done()
	slog.Info("shutting down api server")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	wire.CloseProducer()
	slog.Info("api server stopped")
}
