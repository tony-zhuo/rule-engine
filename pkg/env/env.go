// Package env reads process environment variables with fallback defaults.
// The data-plane binaries (cmd/rule-engine-core, cmd/event-producer,
// cmd/bench) are configured entirely through the environment, so this is
// their single source of parsing behaviour.
package env

import (
	"os"
	"strconv"
	"time"
)

// Str returns the value of key, or def when unset or empty.
func Str(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Int returns key parsed as an integer, or def when unset, empty, or unparsable.
func Int(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Duration returns key parsed with time.ParseDuration (e.g. "30s", "5m"),
// or def when unset, empty, or unparsable.
func Duration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
