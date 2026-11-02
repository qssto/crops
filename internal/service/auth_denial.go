package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 拒绝审计（P1 §6.2 流程 5）：SCOPE_DENIED / ROLE_INSUFFICIENT / 资源不在 Scope 内一律写
// audit_log(CROSS_FARM_DENIED, DENIED)，同时打结构化日志并计数（L7 接 Prometheus as_cross_farm_denied_total）。

// 拒绝原因（响应错误码与审计 summary.code）。
const (
	DenyScope    = "SCOPE_DENIED"          // 路径农场不在授权内，或 token 租户与农场归属不符
	DenyRole     = "ROLE_INSUFFICIENT"     // 权限矩阵未授予该操作
	DenyResource = "RESOURCE_NOT_IN_SCOPE" // IDOR：run / ticket / raw / field 等不属于路径农场（对外仍回 403）
)

// DeniedMetric 是拒绝计数的指标名。
const DeniedMetric = "as_cross_farm_denied_total"

// denialWriteTimeout 是拒绝审计独立短写的上限：脱离请求取消后仍须有界，库慢时不拖住 403 响应。
const denialWriteTimeout = 2 * time.Second

// AccessDenial 是一次访问拒绝。Scope 为零值（SCOPE_DENIED、平台级事件）时审计写在 (0,0)。
type AccessDenial struct {
	Scope         model.Scope
	UserID        uint64
	FarmID        uint64 // 路径 farm_id（平台路由为 0）
	OwnerTenantID uint64 // 路径农场的真实归属租户（只在 token 租户与农场归属不符时填，供排查）
	Code          string
	Op            Operation // 被拒的操作（SCOPE_DENIED 时可空）
	Target        string    // farm:{id} / run:{id} / ticket:{id} / raw:{id} / field:{id}
	Route         string    // METHOD + 路由模板
	RequestID     string
}

// DenialCounter 是拒绝计数接缝（L7 接 Prometheus）；可空。
type DenialCounter interface {
	IncDenied(code string)
}

// DenialStore 独立短写审计（不随业务事务回滚），repository.AuditRepo 实现。
type DenialStore interface {
	Append(ctx context.Context, r model.AuditRecord) error
}

// DenialAuditor 记录访问拒绝。写审计失败只记错误日志，不改变对外的 403。
type DenialAuditor struct {
	store   DenialStore
	clock   clock.Clock
	log     *slog.Logger
	counter DenialCounter
}

// NewDenialAuditor 构造 DenialAuditor；counter 可空，logger 为空取 slog.Default。
func NewDenialAuditor(store DenialStore, c clock.Clock, log *slog.Logger, counter DenialCounter) (*DenialAuditor, error) {
	if store == nil || c == nil {
		return nil, errors.New("service: denial auditor needs store and clock")
	}
	if log == nil {
		log = slog.Default()
	}
	return &DenialAuditor{store: store, clock: c, log: log, counter: counter}, nil
}

// denialSummary 是 CROSS_FARM_DENIED 审计的 payload（只含控制字段）。
type denialSummary struct {
	Code          string    `json:"code"`
	Op            Operation `json:"op,omitempty"`
	FarmID        uint64    `json:"path_farm_id,omitempty"`
	OwnerTenantID uint64    `json:"owner_tenant_id,omitempty"`
	Route         string    `json:"route,omitempty"`
	RequestID     string    `json:"request_id,omitempty"`
}

// Deny 写审计、日志与计数；ctx 取消不影响审计写入，写入受 denialWriteTimeout 约束。
func (a *DenialAuditor) Deny(ctx context.Context, d AccessDenial) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), denialWriteTimeout)
	defer cancel()
	a.log.WarnContext(ctx, "access denied", "metric", DeniedMetric, "code", d.Code, "op", string(d.Op),
		"user_id", d.UserID, "path_farm_id", d.FarmID, "owner_tenant_id", d.OwnerTenantID,
		"tenant_id", d.Scope.TenantID, "farm_id", d.Scope.FarmID,
		"target", d.Target, "route", d.Route, "request_id", d.RequestID)
	if a.counter != nil {
		a.counter.IncDenied(d.Code)
	}
	payload, err := json.Marshal(denialSummary{Code: d.Code, Op: d.Op, FarmID: d.FarmID, OwnerTenantID: d.OwnerTenantID,
		Route: d.Route, RequestID: d.RequestID})
	if err != nil {
		a.log.ErrorContext(ctx, "access denied: encode audit", "err", err)
		return
	}
	at := model.Scope{TenantID: d.Scope.TenantID, FarmID: d.Scope.FarmID}
	rec := repository.NewAuditRecord(at, nil, model.Actor{Type: model.ActorUser, ID: d.UserID},
		model.AuditCrossFarmDenied, d.Target, payload, model.AuditResultDenied, a.clock.Now())
	if err := a.store.Append(ctx, rec); err != nil {
		a.log.ErrorContext(ctx, "access denied: write audit", "code", d.Code, "err", err)
	}
}
