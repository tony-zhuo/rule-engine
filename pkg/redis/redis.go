package redis

import (
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

var (
	once   sync.Once
	client *redis.Client
)

type RedisConfig struct {
	MasterName    string `mapstructure:"master_name"`
	SentinelAddrs string `mapstructure:"sentinel_addrs"`
	Password      string `mapstructure:"password"`
	DB            int    `mapstructure:"db"`
}

func Init(config RedisConfig) {
	once.Do(func() {
		addrs := strings.Split(config.SentinelAddrs, ",")
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    config.MasterName,
			SentinelAddrs: addrs,
			Password:      config.Password,
			DB:            config.DB,
		})
	})
}

func GetClient() *redis.Client {
	return client
}
