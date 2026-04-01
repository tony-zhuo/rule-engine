package config

type App struct {
	Addr string `mapstructure:"addr"`
}

type DB struct {
	DSN string `mapstructure:"dsn"`
}

type Redis struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type Log struct {
	Level string `mapstructure:"level"`
}
