package main

import (
	"log"

	"github.com/tony-zhuo/rule-engine/config"
	"github.com/tony-zhuo/rule-engine/service/bff/apis"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}
	apis.InitRoute(cfg)
}
