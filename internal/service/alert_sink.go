package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 告警审计独立短写的上限：脱离调用方取消后仍须有界，库慢时不拖住业务路径。Fire 可能在 lock:order / lock:scan 内同步调用
// （下发器拦截、无引用扫描），审查修复把缺省从 2s 降到 500ms 并可配（alerts.audit_write_timeout，上限 MaxAlertWriteTimeout）。
const (
	DefaultAlertWriteTimeout = 500 * time.Millisecond
	MaxAlertWriteTimeout     = 500 * time.Millisecond
)

// alertDetailMax 是告警审计 summary.detail 的最大字符数（detail 由调用方保证不含 PII）。
const alertDetailMax = 256

// AuditAlertSink 是 model.AlertSink 的生产实现（P1 L7-B）：
//  1. 计 as_alert_total{alert, level, farm_id}；
//  2. 独立短写 audit_log(ALERT.{name})：target = order:{id}（有 OrderID 时）或 run:{id}，run_id 列有值时填，
//     summary 只含控制字段（告警名、级别、run / order id、detail），result=FAILED（P0）或 DENIED（P1）；
//  3. Error 级结构化日志。
//
// 审计写失败只记 Error 日志（不回调自身，避免告警递归），不 panic、不向调用方返回错误；写入受 timeout 约束。
//
// 平台链回退（L7-A 合并语义）：ALERT 审计按告警 scope 入链，scope 的 audit_chain_head 缺失时 L7-A 发 audit_write_failed，
// 而这条告警自己的审计也会因同一 head 缺失失败。取「通用回退」而非「audit_* 一律写 (0,0)」：按 scope 写失败、且失败发生在
// 截止前（不是超时，库慢时重试只会再超时）、scope 不是 (0,0) 时，在同一截止时间内再写一次平台链 (0,0)，summary 带
// origin{tenant_id, farm_id}，run_id 列置空（不跨 scope 引用 run）。不判断具体错误类型（本分支无 L7-A 的
// ErrAuditChainHeadMissing），任何截止前失败都回退；总耗时仍 ≤ timeout。回退行不出现在农场告警列表（scope 为平台），按原
// tenant / farm 在平台审计里查。
type AuditAlertSink struct {
	audit   AuditAppender // 独立短写（outbox.go 的接口），不随业务事务回滚
	clock   clock.Clock
	log     *slog.Logger
	obs     Observer
	timeout time.Duration
}

var _ model.AlertSink = (*AuditAlertSink)(nil)

// NewAuditAlertSink 构造告警出口；obs 为空时不计指标，logger 为空取 slog.Default；写超时取 DefaultAlertWriteTimeout。
func NewAuditAlertSink(audit AuditAppender, c clock.Clock, log *slog.Logger, obs Observer) (*AuditAlertSink, error) {
	if audit == nil || c == nil {
		return nil, errors.New("service: alert sink needs audit appender and clock")
	}
	if log == nil {
		log = slog.Default()
	}
	if obs == nil {
		obs = NopObserver{}
	}
	return &AuditAlertSink{audit: audit, clock: c, log: log, obs: obs, timeout: DefaultAlertWriteTimeout}, nil
}

// SetWriteTimeout 设置审计写超时（装配期调用）；须在 (0, MaxAlertWriteTimeout] 内。
func (s *AuditAlertSink) SetWriteTimeout(d time.Duration) error {
	if d <= 0 || d > MaxAlertWriteTimeout {
		return fmt.Errorf("service: alert write timeout %v not in (0, %v]", d, MaxAlertWriteTimeout)
	}
	s.timeout = d
	return nil
}

// alertSummary 是 ALERT.* 审计的 summary。
type alertSummary struct {
	Alert   model.AlertName  `json:"alert"`
	Level   model.AlertLevel `json:"level"`
	RunID   uint64           `json:"run_id,omitempty"`
	OrderID uint64           `json:"order_id,omitempty"`
	Detail  string           `json:"detail,omitempty"`
	Origin  *alertOrigin     `json:"origin,omitempty"` // 只在平台链回退行上出现
}

// alertOrigin 是回退到平台链 (0,0) 的告警审计原本所属的 scope。
type alertOrigin struct {
	TenantID uint64 `json:"tenant_id"`
	FarmID   uint64 `json:"farm_id"`
}

// AlertTarget 是告警审计的 target：有作业单时 order:{id}，否则 run:{id}，都没有时为空。
func AlertTarget(a model.Alert) string {
	switch {
	case a.OrderID != 0:
		return "order:" + strconv.FormatUint(a.OrderID, 10)
	case a.RunID != 0:
		return "run:" + strconv.FormatUint(a.RunID, 10)
	}
	return ""
}

func alertResult(l model.AlertLevel) string {
	if l == model.AlertP0 {
		return model.AuditResultFailed
	}
	return model.AuditResultDenied
}

// Fire 实现 model.AlertSink。
func (s *AuditAlertSink) Fire(ctx context.Context, a model.Alert) {
	defer func() {
		if r := recover(); r != nil {
			s.log.ErrorContext(ctx, "alert sink panic recovered", "alert", a.Name, "panic", r)
		}
	}()
	s.obs.AlertFired(a)
	s.log.ErrorContext(ctx, "alert", "alert", string(a.Name), "level", string(a.Level), "tenant_id", a.Scope.TenantID,
		"farm_id", a.Scope.FarmID, "run_id", a.RunID, "order_id", a.OrderID, "detail", a.Detail)
	sum := alertSummary{Alert: a.Name, Level: a.Level, RunID: a.RunID, OrderID: a.OrderID,
		Detail: truncateRunes(a.Detail, alertDetailMax)}
	scope := model.Scope{TenantID: a.Scope.TenantID, FarmID: a.Scope.FarmID}
	var runID *uint64
	if a.RunID != 0 {
		id := a.RunID
		runID = &id
	}
	at := s.clock.Now()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
	defer cancel()
	err := s.appendAlert(wctx, a, scope, runID, sum, at)
	if err == nil {
		return
	}
	s.log.ErrorContext(ctx, "alert: write audit failed", "alert", string(a.Name), "level", string(a.Level),
		"tenant_id", scope.TenantID, "farm_id", scope.FarmID, "run_id", a.RunID, "order_id", a.OrderID, "err", err)
	if (scope.TenantID == 0 && scope.FarmID == 0) || wctx.Err() != nil {
		return // 已是平台链，或已到截止（库慢，重试只会再超时）
	}
	sum.Origin = &alertOrigin{TenantID: scope.TenantID, FarmID: scope.FarmID}
	if ferr := s.appendAlert(wctx, a, model.Scope{}, nil, sum, at); ferr != nil {
		s.log.ErrorContext(ctx, "alert: platform-chain fallback failed", "alert", string(a.Name), "level", string(a.Level),
			"tenant_id", scope.TenantID, "farm_id", scope.FarmID, "err", ferr)
		return
	}
	s.log.WarnContext(ctx, "alert: audit written to platform chain", "alert", string(a.Name),
		"tenant_id", scope.TenantID, "farm_id", scope.FarmID)
}

// appendAlert 编码 summary 并独立短写一行 ALERT.{name} 审计（经 AuditAppender，即 AuditRepo.Append）。
func (s *AuditAlertSink) appendAlert(ctx context.Context, a model.Alert, scope model.Scope, runID *uint64,
	sum alertSummary, at time.Time) error {
	payload, err := json.Marshal(sum)
	if err != nil {
		return fmt.Errorf("service: encode alert audit: %w", err)
	}
	rec := repository.NewAuditRecord(scope, runID, model.Actor{Type: model.ActorSystem}, a.Name.AuditAction(),
		AlertTarget(a), payload, alertResult(a.Level), at)
	if err := s.audit.Append(ctx, rec); err != nil {
		return fmt.Errorf("service: append alert audit: %w", err)
	}
	return nil
}
