package logs

import "log/slog"

type LogConfig struct {
	Level string `mapstructure:"level"`
}

func GetLogger() *slog.Logger {
	return slog.Default()
}
