package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// P1 L4-B：回执处理（§6.5 技术实现第 5-9 条）。回执 / 补偿属下发路径：回执入库与状态迁移同一事务、持 lock:order，
// 事务失败整体回滚；webhook 与超时扫描的跨农场反查只经 SystemScope。

// 回执侧错误。
var (
	// ErrOrderOpsNotConfigured 表示 OrderOpsService 依赖缺失。
	ErrOrderOpsNotConfigured = errors.New("service: order ops not configured")
	// ErrReceiptAuth 表示回执签名不符或时间窗过期（401）。
	ErrReceiptAuth = errors.New("RECEIPT_SIGNATURE_INVALID")
	// ErrReceiptUnknownJob 表示按 (device_type, device_job_id) 暂时反查不到尝试（503，设备平台稍后重投）。
	ErrReceiptUnknownJob = errors.New("RECEIPT_JOB_UNKNOWN")
	// ErrReconcileState 表示作业单最后一次尝试不在待对账列表（409）。
	ErrReconcileState = errors.New("RECONCILE_NOT_PENDING")
	// ErrCancelState 表示作业单当前状态不可中止（终态，或已下发但没有平台任务号，需人工处理；409）。
	ErrCancelState = errors.New("CANCEL_NOT_ALLOWED")
)

// 回执侧默认值。
const (
	// DefaultReceiptWindow 是非终态回执顺延 receipt_due_at 的时长。
	DefaultReceiptWindow = 30 * time.Minute
	// receiptLateFor 是无法匹配的回执转人工前的等待：device_ts 超过它仍反查不到即存证 + RECEIPT_UNMATCHED（§6.5）。
	receiptLateFor = 10 * time.Minute
	// receiptIdemPrefix 区分 idem:receipt 值的版本。
	receiptIdemPrefix = "v1"
)

// 回执侧 SystemScope：webhook 反查与回执超时扫描（archtest 白名单只放行本文件构造）。
var (
	receiptLookupScope = repository.NewSystemScope("receipt_lookup")
	receiptDueScope    = repository.NewSystemScope(rediskey.ScanReceiptDue)
)

// ReceiptStore 是回执 / 对账 / 中止的仓储能力（app 组合 DeviceReceiptRepo、DispatchAttemptRepo、DispatchRepo 实现）；
// *Tx 方法只在调用方事务内使用。
type ReceiptStore interface {
	OrderForDispatch(ctx context.Context, db *repository.DB, ref model.OrderRef) (repository.DispatchOrder, error)
	AttemptTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64) (model.DispatchAttempt, error)
	LatestAttemptTx(ctx context.Context, tx *repository.DB, ref model.OrderRef) (model.DispatchAttempt, error)
	InsertReceiptTx(ctx context.Context, tx *repository.DB, r repository.ReceiptRow) (bool, error)
	UpdateReceiptTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64, rowVer int,
		status model.AttemptStatus, kind model.FailureKind, doneAreaMu *float64, receiptDueAt, now time.Time) error
	MarkNoReceiptTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, id uint64, rowVer int, now time.Time) error
	SetDoneAreaTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, doneAreaMu float64, now time.Time) error
	InsertWorkDoneMemoryTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, fieldID uint64, summary string, at time.Time) error
	InsertFieldHistoryOutboxTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, ticketID uint64, payload []byte, now time.Time) error
	InsertRetryDispatchOutboxTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, ticketID uint64, attempt int,
		payload []byte, now time.Time) error
	InsertCancelOutboxTx(ctx context.Context, tx *repository.DB, ref model.OrderRef, ticketID uint64, payload []byte, now time.Time) (bool, error)
	PendingReconcile(ctx context.Context, scope model.Scope, limit int) ([]model.DispatchAttempt, error)
}

// CancelTicketStore 是中止未下发单时关票与收尾 run 的仓储能力（repository.DecisionRepo 实现），只在中止事务内使用。
type CancelTicketStore interface {
	LiveTicketsTx(ctx context.Context, tx *repository.DB, ref model.OrderRef) ([]repository.TicketRow, error)
	SupersedeLiveTicketTx(ctx context.Context, tx *repository.DB, t repository.TicketRow, now time.Time) error
	CloseWaitingRunTx(ctx context.Context, tx *repository.DB, scope model.Scope, runID uint64, reason model.ReasonCode,
		now time.Time) (bool, error)
}

// ReceiptLookup 是跨农场反查（须持 SystemScope）：webhook 按任务号找归属，超时扫描取过期尝试。
type ReceiptLookup interface {
	ByJob(ctx context.Context, sys repository.SystemScope, deviceType, jobID string) (repository.AttemptOwner, error)
	DueAttempts(ctx context.Context, sys repository.SystemScope, now time.Time) ([]repository.DueAttempt, error)
}

// DeviceTarget 是一次回执侧设备调用的定位：app 按 DeviceType 取 Adapter，工单信息进 trace。
type DeviceTarget struct {
	DeviceType string
	Ref        model.OrderRef
	RunID      uint64
}

// ReceiptDevices 是回执侧的设备能力（service 不 import adapter，app 桥接 adapter.Registry）。
type ReceiptDevices interface {
	QueryStatus(ctx context.Context, t DeviceTarget, idemKey, jobID string) (model.DeviceReceipt, error)
	Cancel(ctx context.Context, t DeviceTarget, jobID string) (model.DeviceReceipt, error)
}

// ReceiptIdem 是 idem:receipt 快速去重（repository.RedisIdem 实现）；丢失由 uk_receipt 兜底。
type ReceiptIdem interface {
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Get(ctx context.Context, key string) (string, bool, error)
}

// ReceiptDenier 记录跨农场回执拒绝（DenialAuditor 实现，复用 CROSS_FARM_DENIED 审计）。
type ReceiptDenier interface {
	Deny(ctx context.Context, d AccessDenial)
}

// FarmTenantLookup 按农场找归属租户（UserRoleRepo 实现）：无法匹配的回执原文存证时定位 raw_object 的 (tenant, farm)。
type FarmTenantLookup interface {
	FarmTenants(ctx context.Context, farmID uint64) ([]uint64, error)
}

// OrderOpsDeps 是 OrderOpsService 的依赖。Secret 为空时 Webhook 一律 ErrReceiptAuth（app 不挂路由）；
// MaxAttempts 必填（config dispatch.max_attempts，与下发器同一个值，须 ≥ 1）；ReceiptWindow / ScanInterval 为零取默认值。
type OrderOpsDeps struct {
	Store         ReceiptStore
	Tickets       CancelTicketStore
	Lookup        ReceiptLookup
	Tx            TxRunner
	Orders        OrderTransitioner
	Devices       ReceiptDevices
	Idem          ReceiptIdem
	Locks         KeyLocker
	Keys          rediskey.Keys
	Raw           agent.RawStore
	Audit         DenialStore     // 独立短写：RECEIPT_UNMATCHED
	AuditTx       AuditTxAppender // 事务内：RECONCILE / ORDER_CANCEL
	Denials       ReceiptDenier
	Tenants       FarmTenantLookup
	History       FieldHistoryWriter
	Secret        []byte
	MaxAttempts   int
	ReceiptWindow time.Duration
	ScanInterval  time.Duration
	Clock         clock.Clock
	Logger        *slog.Logger
}

// OrderOpsService 处理设备回执、回执超时扫描、人工对账、中止补偿与 field.history 回写。
type OrderOpsService struct{ d OrderOpsDeps }

// NewOrderOpsService 校验依赖并构造。
func NewOrderOpsService(d OrderOpsDeps) (*OrderOpsService, error) {
	if d.Store == nil || d.Tickets == nil || d.Lookup == nil || d.Tx == nil || d.Orders == nil || d.Devices == nil || d.Idem == nil ||
		d.Locks == nil || d.Raw == nil || d.Audit == nil || d.AuditTx == nil || d.Denials == nil || d.Tenants == nil ||
		d.History == nil || d.Clock == nil || d.Logger == nil || d.Keys.Prefix() == "" {
		return nil, fmt.Errorf("%w: all deps except secret / tunables are required", ErrOrderOpsNotConfigured)
	}
	if d.MaxAttempts < 1 {
		return nil, fmt.Errorf("%w: max_attempts %d must be >= 1 (config dispatch.max_attempts)", ErrOrderOpsNotConfigured,
			d.MaxAttempts)
	}
	if d.ReceiptWindow <= 0 {
		d.ReceiptWindow = DefaultReceiptWindow
	}
	if d.ScanInterval <= 0 {
		d.ScanInterval = DefaultScanInterval
	}
	return &OrderOpsService{d: d}, nil
}

// idemValue 是 idem:receipt 的值：v1:{tenant}:{farm}:{outcome}（归属不符不当作重放，交给 uk_receipt 判定）。
func idemValue(ref model.OrderRef, o model.ReceiptOutcome) string {
	return strings.Join([]string{receiptIdemPrefix, strconv.FormatUint(ref.TenantID, 10), strconv.FormatUint(ref.FarmID, 10),
		string(o)}, ":")
}

// parseIdem 解析 idem:receipt 的值；归属与 ref 一致才返回 ok。
func parseIdem(v string, ref model.OrderRef) (model.ReceiptOutcome, bool) {
	parts := strings.Split(v, ":")
	if len(parts) != 4 || parts[0] != receiptIdemPrefix ||
		parts[1] != strconv.FormatUint(ref.TenantID, 10) || parts[2] != strconv.FormatUint(ref.FarmID, 10) {
		return "", false
	}
	return model.ReceiptOutcome(parts[3]), true
}
