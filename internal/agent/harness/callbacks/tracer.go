package callbacks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Span 种类。
const (
	SpanStep = "step"
	SpanLLM  = "llm"
)

// Span 是一次步骤 / 模型调用的观测记录：只带 ID、耗时、token 与状态码（不带原文）。
type Span struct {
	Kind             string
	Name             string
	RunID            uint64
	StepID           string
	Status           string
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	Error            string
}

// Tracer 是 trace 出口小接口（默认 SlogTracer；接 OTel 留到依赖获批后再加实现）。
type Tracer interface {
	Record(ctx context.Context, s Span)
}

type nopTracer struct{}

func (nopTracer) Record(context.Context, Span) {}

// SlogTracer 把 Span 写成一条 slog 记录；属性经 Mask（由 app 注入 pii.MaskAttrs，本包不 import guardrail）。
type SlogTracer struct {
	Log  *slog.Logger
	Mask func([]slog.Attr) []slog.Attr
}

// Record 实现 Tracer。
func (t SlogTracer) Record(ctx context.Context, s Span) {
	if t.Log == nil {
		return
	}
	attrs := []slog.Attr{slog.String("span", s.Kind), slog.String("name", s.Name), slog.Uint64("run_id", s.RunID),
		slog.String("step_id", s.StepID), slog.String("status", s.Status), slog.Int64("latency_ms", s.LatencyMS)}
	if s.Kind == SpanLLM {
		attrs = append(attrs, slog.Int("prompt_tokens", s.PromptTokens), slog.Int("completion_tokens", s.CompletionTokens))
	}
	if s.Error != "" {
		attrs = append(attrs, slog.String("error", s.Error))
	}
	if t.Mask != nil {
		attrs = t.Mask(attrs)
	}
	t.Log.LogAttrs(ctx, slog.LevelInfo, "trace", attrs...)
}

func marshal(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("callbacks: encode payload: %w", err)
	}
	return b, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
