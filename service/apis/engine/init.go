package engine

import (
	"github.com/tony-zhuo/rule-engine/config"
)

func InitRoute(cfg *config.Config) {
	ApiRegister(cfg)
}
