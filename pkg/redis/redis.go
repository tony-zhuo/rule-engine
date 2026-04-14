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
	PoolSize      int    `mapstructure:"pool_size"`       // max open connections (default: 10 * NumCPU)
	MinIdleConns  int    `mapstructure:"min_idle_conns"`  // warm connections kept ready (default: 0)
}

func Init(config RedisConfig) {
	once.Do(func() {
		addrs := strings.Split(config.SentinelAddrs, ",")
		opts := &redis.FailoverOptions{
			MasterName:    config.MasterName,
			SentinelAddrs: addrs,
			Password:      config.Password,
			DB:            config.DB,
		}
		if config.PoolSize > 0 {
			opts.PoolSize = config.PoolSize
		}
		if config.MinIdleConns > 0 {
			opts.MinIdleConns = config.MinIdleConns
		}
		client = redis.NewFailoverClient(opts)
	})
}

func GetClient() *redis.Client {
	return client
}
