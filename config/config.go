package config

import (
	"strings"

	"github.com/spf13/viper"
	"github.com/tony-zhuo/rule-engine/pkg/db"
	"github.com/tony-zhuo/rule-engine/pkg/logs"
	"github.com/tony-zhuo/rule-engine/pkg/redis"
)

// Config holds the shared configuration. The legacy worker's Kafka section was
// removed in Task N — the in-memory engine reads Kafka settings (BACKEND=kafka,
// KAFKA_BROKERS, etc.) directly from env in cmd/rule-engine-core /
// cmd/event-producer, not from this shared config struct.
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
