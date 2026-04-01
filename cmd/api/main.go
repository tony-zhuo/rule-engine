package main

import (
	"log"

	"github.com/tony-zhuo/rule-engine/config"
	engine "github.com/tony-zhuo/rule-engine/service/apis/engine"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("failed to load config: ", err)
	}
	engine.InitRoute(cfg)
}
