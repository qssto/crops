package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// P1 L7-B 看板扩展事实行（口径见 docs/metrics.md「P1-L7-B 运营指标」）：下发尝试、作业单关键迁移与当前审批积压。
const (
	// 窗口内发出的 REAL 下发尝试；accepted 表示设备平台已受理（返回 device_job_id）。
	sqlMetricAttempts = `SELECT device_job_id IS NOT NULL AS accepted, sent_at FROM dispatch_attempt
 WHERE tenant_id=? AND farm_id=? AND mode='REAL' AND sent_at>=? AND sent_at<? ORDER BY id`
	// 窗口内的作业单关键迁移：终态（DONE / PARTIALLY_DONE / FAILED）、EXPIRED 与复检退回（reason RECHECK_FAILED）。
	// work_order_transition 无 created_at 索引，按 idx_order 的 (tenant_id, farm_id) 前缀过滤（单农场量级可接受）。
	sqlMetricTransitions = `SELECT w.to_status, w.reason_code, o.dispatch_mode, w.created_at FROM work_order_transition w
 JOIN work_order o ON o.id=w.order_id AND o.tenant_id=w.tenant_id AND o.farm_id=w.farm_id
 WHERE w.tenant_id=? AND w.farm_id=? AND w.created_at>=? AND w.created_at<?
   AND (w.to_status IN ('DONE','PARTIALLY_DONE','FAILED','EXPIRED') OR w.reason_code='RECHECK_FAILED') ORDER BY w.id`
	// 当前 OPEN 的 ORDER 票（查询时刻快照，不随时间窗）。
	sqlMetricOpenTickets = `SELECT COUNT(*) AS n FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status='OPEN'`
)

// MetricAttempt 是窗口内的一次 REAL 下发尝试。
type MetricAttempt struct {
	Accepted bool
	SentAt   time.Time
}

// MetricTransition 是窗口内的一条作业单关键迁移。
type MetricTransition struct {
	To           model.WorkOrderStatus
	ReasonCode   model.ReasonCode
	DispatchMode string
	CreatedAt    time.Time
}

// opsFacts 读 L7-B 扩展事实行并填入 f。
func (r *MetricsRepo) opsFacts(ctx context.Context, scope model.Scope, from, to time.Time, f *MetricFacts) error {
	win := []any{scope.TenantID, scope.FarmID, from, to}
	db := r.db.WithContext(ctx)
	if err := db.Raw(sqlMetricAttempts, win...).Scan(&f.Attempts).Error; err != nil {
		return fmt.Errorf("repository: metric facts dispatch_attempt: %w", err)
	}
	var rows []struct {
		ToStatus     string
		ReasonCode   sql.NullString
		DispatchMode string
		CreatedAt    time.Time
	}
	if err := db.Raw(sqlMetricTransitions, win...).Scan(&rows).Error; err != nil {
		return fmt.Errorf("repository: metric facts work_order_transition: %w", err)
	}
	for _, x := range rows {
		f.Transitions = append(f.Transitions, MetricTransition{To: model.WorkOrderStatus(x.ToStatus),
			ReasonCode: model.ReasonCode(x.ReasonCode.String), DispatchMode: x.DispatchMode, CreatedAt: x.CreatedAt})
	}
	var open struct{ N int }
	if err := db.Raw(sqlMetricOpenTickets, scope.TenantID, scope.FarmID).Scan(&open).Error; err != nil {
		return fmt.Errorf("repository: metric facts open tickets: %w", err)
	}
	f.OpenTickets = open.N
	return nil
}
