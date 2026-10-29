package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// Handler 是应用日志出口：输出前对消息、全部属性（含分组、WithAttrs 预置属性）与错误链脱敏，再交给下游。
type Handler struct {
	next slog.Handler
}

// NewHandler 包装下游 Handler。
func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }

// Enabled 委托下游。
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

// Handle 重建记录：消息与每个属性经脱敏后交给下游（保留时间、级别与 PC）。
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Mask(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(MaskAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

// WithAttrs 预置属性先脱敏。
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(MaskAttrs(as))}
}

// WithGroup 委托下游。
func (h *Handler) WithGroup(name string) slog.Handler { return &Handler{next: h.next.WithGroup(name)} }

// Unwrap 返回下游 Handler（评测按链查找日志捕获器）。
func (h *Handler) Unwrap() slog.Handler { return h.next }

// MaskAttrs 是属性脱敏（应用日志与 OTel span 属性共用）：返回新切片，不改入参。
func MaskAttrs(as []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = MaskAttr(a)
	}
	return out
}

// MaskAttr 脱敏单个属性：字符串掩码；坐标键下的浮点截断到 0.01°；形似敏感号码的整数改为掩码字符串；
// error 取完整错误链文本后掩码；其他任意值经 JSON 编码后按结构掩码（编码失败退化为 %+v 文本）。
func MaskAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Mask(v.String()))
	case slog.KindGroup:
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(MaskAttrs(v.Group())...)}
	case slog.KindFloat64:
		if IsCoordKey(a.Key) {
			s := strconv.FormatFloat(v.Float64(), 'f', -1, 64)
			if decimals(s) >= minCoordDecimals {
				return slog.String(a.Key, TruncateCoord(s))
			}
		}
		return slog.Attr{Key: a.Key, Value: v}
	case slog.KindInt64:
		return maskInt(a.Key, strconv.FormatInt(v.Int64(), 10), v)
	case slog.KindUint64:
		return maskInt(a.Key, strconv.FormatUint(v.Uint64(), 10), v)
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: maskAny(v.Any())}
	default: // bool / duration / time 不含敏感信息
		return slog.Attr{Key: a.Key, Value: v}
	}
}

func maskInt(key, s string, v slog.Value) slog.Attr {
	if sensitiveDigits(s) {
		return slog.String(key, maskDigits(s))
	}
	return slog.Attr{Key: key, Value: v}
}

func maskAny(x any) slog.Value {
	switch t := x.(type) {
	case nil:
		return slog.AnyValue(nil)
	case error:
		return slog.StringValue(Mask(t.Error()))
	case json.RawMessage:
		return slog.AnyValue(MaskJSON(t))
	case []byte:
		return slog.StringValue(Mask(string(t)))
	case fmt.Stringer:
		return slog.StringValue(Mask(t.String()))
	}
	b, err := json.Marshal(x)
	if err != nil {
		return slog.StringValue(Mask(fmt.Sprintf("%+v", x)))
	}
	var out any
	dec := json.NewDecoder(bytes.NewReader(MaskJSON(b)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return slog.StringValue(Mask(string(b)))
	}
	return slog.AnyValue(out)
}
