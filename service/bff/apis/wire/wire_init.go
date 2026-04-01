package wire

import (
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	"gorm.io/gorm"
)

var ConfigSet = wire.NewSet(provideGormDB, provideRedisClient)

func provideGormDB(cfg *config.Config) *gorm.DB {
	pkgdb.Init(cfg.DB)
	return pkgdb.GetDB()
}

func provideRedisClient(cfg *config.Config) *redis.Client {
	pkgredis.Init(cfg.Redis)
	return pkgredis.GetClient()
}
