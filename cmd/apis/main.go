package main

import (
	"log"

	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	"github.com/tony-zhuo/rule-engine/service/bff/apis"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}

	pkgdb.Init(cfg.DB)
	pkgredis.Init(cfg.Redis)

	apis.InitRoute(cfg)
}
