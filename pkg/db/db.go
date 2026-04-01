package db

import (
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	once     sync.Once
	instance *gorm.DB
)

type DBConfig struct {
	DSN string `mapstructure:"dsn"`
}

func Init(config DBConfig) {
	once.Do(func() {
		db, err := gorm.Open(postgres.Open(config.DSN), &gorm.Config{})
		if err != nil {
			panic("failed to connect database: " + err.Error())
		}
		instance = db
	})
}

func GetDB() *gorm.DB {
	return instance
}
