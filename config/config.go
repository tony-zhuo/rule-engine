package config

import (
	"strings"

	"github.com/spf13/viper"
	"github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/pkg/logs"
)

// Config is the file-based configuration, used by cmd/apis. The engine binaries
// (cmd/rule-engine-core, cmd/event-producer) are configured through environment
// variables instead, so their MQ settings deliberately do not appear here.
type Config struct {
	App App            `mapstructure:"app"`
	DB  db.DBConfig    `mapstructure:"db"`
	Log logs.LogConfig `mapstructure:"log"`
}

func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

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
