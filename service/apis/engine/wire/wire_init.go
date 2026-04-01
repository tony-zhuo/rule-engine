package wire

import (
	"github.com/google/wire"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	"github.com/tony-zhuo/rule-engine/service/apis/engine/initialize"

	"github.com/redis/go-redis/v9"
)

var ConfigSet = wire.NewSet(provideRedis)

func provideRedis(_ *initialize.Conf) *redis.Client {
	return pkgredis.GetClient()
}
