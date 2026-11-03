package service

import (
	"context"
	"log/slog"

	"agri-steward/internal/model"
)

// LogAlertSink 是缺省告警实现（P1 L7 接缝）：只写 Error 级结构化日志。L7-B 用指标 + 审计的实现替换装配。
type LogAlertSink struct{ Logger *slog.Logger }

var _ model.AlertSink = LogAlertSink{}

// Fire 实现 model.AlertSink。
func (s LogAlertSink) Fire(ctx context.Context, a model.Alert) {
	if s.Logger == nil {
		return
	}
	s.Logger.ErrorContext(ctx, "alert", "alert", a.Name, "level", a.Level, "tenant_id", a.Scope.TenantID,
		"farm_id", a.Scope.FarmID, "run_id", a.RunID, "order_id", a.OrderID, "detail", a.Detail)
}
