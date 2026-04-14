package wire

import (
	"github.com/google/wire"
	goredis "github.com/redis/go-redis/v9"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	"gorm.io/gorm"
)

var ConfigSet = wire.NewSet(provideGormDB, provideRedisClient)

func provideGormDB() *gorm.DB {
	return pkgdb.GetDB()
}

func provideRedisClient() *goredis.Client {
	return pkgredis.GetClient()
}
