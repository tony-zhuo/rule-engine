package engine

import (
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"
)

func Init() {
	initialize.InitConf()
}

func InitRoute() {
	Init()
	ApiRegister()
}
