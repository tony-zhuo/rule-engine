package logs

import "log/slog"

func GetLogger() *slog.Logger {
	return slog.Default()
}
