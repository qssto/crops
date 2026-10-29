package app

import (
	"fmt"
	"io"
	"log/slog"

	"agri-steward/internal/agent/guardrail/pii"
)

// NewLogger 返回统一的 slog JSON 日志，外层是脱敏 pii.Handler（日志出口唯一替换点，P0 §6.6 出口①）。
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	return NewTappedLogger(w, level, nil)
}

// NewTappedLogger 同 NewLogger；tap 非空时包装在脱敏层与 JSON 输出之间（评测按 trial 捕获脱敏后的日志）。
func NewTappedLogger(w io.Writer, level string, tap func(slog.Handler) slog.Handler) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("app: log level %q: %w", level, err)
	}
	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv})
	if tap != nil {
		h = tap(h)
	}
	return slog.New(pii.NewHandler(h)), nil
}
