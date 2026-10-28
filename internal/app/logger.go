package app

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger 返回统一的 slog JSON 日志。L5 在此处把 JSON handler 包进脱敏 slog.Handler（日志出口唯一替换点）。
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("app: log level %q: %w", level, err)
	}
	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})
	return slog.New(h), nil
}
