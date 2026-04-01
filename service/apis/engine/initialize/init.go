package initialize

import "github.com/tony-zhuo/rule-engine/config"

type Conf struct {
	App   config.App   `mapstructure:"app"`
	Redis config.Redis `mapstructure:"redis"`
	Log   config.Log   `mapstructure:"log"`
}

var conf *Conf

func InitConf() *Conf {
	conf = &Conf{
		App: config.App{Addr: ":8080"},
	}
	return conf
}

func GetConf() *Conf {
	return conf
}
