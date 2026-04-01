package config

import (
	"strings"

	"github.com/spf13/viper"
	"github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/pkg/logs"
	"github.com/tony-zhuo/rule-engine/pkg/redis"
)

type Config struct {
	App   App               `mapstructure:"app"`
	DB    db.DBConfig       `mapstructure:"db"`
	Redis redis.RedisConfig `mapstructure:"redis"`
	Log   logs.LogConfig    `mapstructure:"log"`
}

func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	viper.SetDefault("app.addr", ":8080")
	viper.SetDefault("redis.addr", "localhost:6379")
	viper.SetDefault("redis.db", 0)
	viper.SetDefault("log.level", "info")

	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	cfg := &Config{}
	if err := viper.Unmarshal(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
