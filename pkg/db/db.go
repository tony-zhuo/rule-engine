package db

import (
	"fmt"
	"sync"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	once     sync.Once
	instance *gorm.DB
)

type DBConfig struct {
	Host               string `mapstructure:"host"`
	User               string `mapstructure:"user"`
	Password           string `mapstructure:"password"`
	DBName             string `mapstructure:"dbname"`
	Port               int    `mapstructure:"port"`
	SSLMode            string `mapstructure:"sslmode"`
	TimeZone           string `mapstructure:"timezone"`
	MaxOpenConns       int    `mapstructure:"max_open_conns"`
	MaxIdleConns       int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetimeSec int    `mapstructure:"conn_max_lifetime_sec"`
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%d sslmode=%s TimeZone=%s",
		c.Host, c.User, c.Password, c.DBName, c.Port, c.SSLMode, c.TimeZone,
	)
}

func Init(config DBConfig) {
	once.Do(func() {
		db, err := gorm.Open(postgres.Open(config.DSN()), &gorm.Config{
			PrepareStmt: true,
		})
		if err != nil {
			panic("failed to connect database: " + err.Error())
		}

		sqlDB, err := db.DB()
		if err != nil {
			panic("failed to get underlying sql.DB: " + err.Error())
		}

		maxOpen := config.MaxOpenConns
		if maxOpen == 0 {
			maxOpen = 25
		}
		maxIdle := config.MaxIdleConns
		if maxIdle == 0 {
			maxIdle = 10
		}
		lifetimeSec := config.ConnMaxLifetimeSec
		if lifetimeSec == 0 {
			lifetimeSec = 300
		}

		sqlDB.SetMaxOpenConns(maxOpen)
		sqlDB.SetMaxIdleConns(maxIdle)
		sqlDB.SetConnMaxLifetime(time.Duration(lifetimeSec) * time.Second)

		instance = db
	})
}

func GetDB() *gorm.DB {
	return instance
}
