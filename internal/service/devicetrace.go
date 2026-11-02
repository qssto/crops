package service

import (
	"context"
	"log/slog"

	"agri-steward/internal/model"
)

// SpanDevice 是设备调用 trace 的 span 名（与主链路 trace 同为 msg="trace" 的 slog 记录）。
const SpanDevice = "device"

// DeviceTraceLog 是 adapter.CallRecorder 的实现（P1 §6.3 功能任务 6）：每次设备调用写一条 trace 日志，
// 带 tool_call_id / device_type / order_id / 耗时 / 回执码。只依赖 model 类型，不 import adapter。
type DeviceTraceLog struct {
	Log *slog.Logger
}

// RecordDeviceCall 写一条 span=device 的 trace；Log 为空时不记录。出错的调用记 Warn。
func (d DeviceTraceLog) RecordDeviceCall(ctx context.Context, t model.DeviceCallTrace) {
	if d.Log == nil {
		return
	}
	attrs := []slog.Attr{slog.String("span", SpanDevice), slog.String("tool_call_id", t.ToolCallID),
		slog.String("name", t.Method), slog.String("device_type", t.DeviceType), slog.Uint64("tenant_id", t.TenantID),
		slog.Uint64("farm_id", t.FarmID), slog.Uint64("order_id", t.OrderID), slog.Uint64("run_id", t.RunID),
		slog.String("idem_key", t.IdemKey), slog.String("device_job_id", t.DeviceJobID),
		slog.String("status", string(t.Status)), slog.String("code", t.Code), slog.Int64("latency_ms", t.Latency.Milliseconds())}
	if t.CommandRawID != 0 {
		attrs = append(attrs, slog.Uint64("command_raw_id", t.CommandRawID))
	}
	level := slog.LevelInfo
	if t.Error != "" {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error", t.Error))
	}
	d.Log.LogAttrs(ctx, level, "trace", attrs...)
}
