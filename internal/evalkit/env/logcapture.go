package env

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
)

// LogCapture 是一个 trial 的应用日志捕获（pii_clean 扫描用）。Tap 作为 app.NewLogger 的包装函数插在
// 脱敏层与 JSON 输出之间：记录原样交给下游，同时另写一份 JSON 行到内存，因此捕获的是脱敏后实际写出的内容。
type LogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// NewLogCapture 返回空捕获器。
func NewLogCapture() *LogCapture { return &LogCapture{} }

// Tap 包装下游 Handler；同一 LogCapture 的全部派生 Handler（WithAttrs / WithGroup）写同一缓冲。
func (c *LogCapture) Tap(next slog.Handler) slog.Handler {
	// 级别由下游 Enabled 决定；副本不再过滤。
	return &captureHandler{next: next, mirror: slog.NewJSONHandler(captureWriter{c}, &slog.HandlerOptions{Level: slog.LevelDebug}), c: c}
}

// Logs 返回已捕获日志的副本（无日志时为空切片，非 nil：nil 表示「未记录」）。
func (c *LogCapture) Logs() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte{}, c.buf.Bytes()...)
}

// captureWriter 串行写入缓冲（slog JSONHandler 每条记录一次 Write）。
type captureWriter struct{ c *LogCapture }

func (w captureWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}

// captureHandler 把记录同时交给下游与内存副本。
type captureHandler struct {
	next   slog.Handler
	mirror slog.Handler
	c      *LogCapture
}

func (h *captureHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	return errors.Join(h.mirror.Handle(ctx, r.Clone()), h.next.Handle(ctx, r))
}

func (h *captureHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &captureHandler{next: h.next.WithAttrs(as), mirror: h.mirror.WithAttrs(as), c: h.c}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{next: h.next.WithGroup(name), mirror: h.mirror.WithGroup(name), c: h.c}
}

// Logs 让 Runner 沿 Handler 链（Unwrap）找到捕获器。
func (h *captureHandler) Logs() []byte { return h.c.Logs() }
