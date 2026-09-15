// Package observability provides logger construction and safe error
// classification. Errors surfaced to logs must never embed repository URLs,
// raw YAML, credential fragments, or remote-controlled stderr.
package observability

import (
	"fmt"
	"log/slog"
	"strings"
)

// ParseLevel maps the configured log level name to slog.Level. Only the four
// documented levels are accepted.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("unsupported log level %q", name)
}

// NewLogger builds the process-wide JSON logger writing to stdout with the
// configured level.
func NewLogger(level string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(stdoutWriter, &slog.HandlerOptions{
		Level: lvl,
	})), nil
}
