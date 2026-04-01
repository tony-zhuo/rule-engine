package wire

import (
	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/google/wire"
	goredis "github.com/redis/go-redis/v9"
	"github.com/tony-zhuo/rule-engine/config"
	pkgdb "github.com/tony-zhuo/rule-engine/pkg/db"
	pkgkafka "github.com/tony-zhuo/rule-engine/pkg/kafka"
	pkgredis "github.com/tony-zhuo/rule-engine/pkg/redis"
	"gorm.io/gorm"
)

var ConfigSet = wire.NewSet(provideGormDB, provideRedisClient, provideKafkaProducer)

func provideGormDB() *gorm.DB {
	return pkgdb.GetDB()
}

func provideRedisClient() *goredis.Client {
	return pkgredis.GetClient()
}

func provideKafkaProducer(cfg *config.Config) *kafka.Producer {
	p, err := pkgkafka.NewProducer(cfg.Kafka)
	if err != nil {
		panic("failed to create kafka producer: " + err.Error())
	}
	return p
}
