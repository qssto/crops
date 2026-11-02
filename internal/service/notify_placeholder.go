package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// NotifyPlaceholder 是 outbox notify.send 的占位处理器（P1-L4-A；真实通道在 L5）：只写结构化日志 + 审计 NOTIFY_SEND
// 后返回 nil（置 DONE），保证 Reopen 写的「复检退回」通知不进 DEAD。审计写失败返回错误，按 outbox 退避重试。
type NotifyPlaceholder struct {
	Audit  AuditAppender
	Clock  clock.Clock
	Logger *slog.Logger
}

// notifyEnvelope 是 notify.send payload 的公共字段（kind 区分通知类型，如 RECHECK_RETURNED）。
type notifyEnvelope struct {
	Kind    string `json:"kind"`
	OrderID uint64 `json:"order_id"`
	RunID   uint64 `json:"run_id"`
}

// Handle 实现 OutboxHandler。payload 不可解析 → ErrBadOutboxPayload（退避后 DEAD，留人工查看）。
func (n NotifyPlaceholder) Handle(ctx context.Context, msg repository.OutboxMessage) error {
	if n.Audit == nil || n.Clock == nil || n.Logger == nil {
		return fmt.Errorf("service: notify.send: %w", ErrDispatchNotConfigured)
	}
	var env notifyEnvelope
	if err := json.Unmarshal(msg.Payload, &env); err != nil || env.Kind == "" {
		return fmt.Errorf("%w: notify.send outbox %d: kind required (%v)", ErrBadOutboxPayload, msg.ID, err)
	}
	var runID *uint64
	if env.RunID != 0 {
		runID = &env.RunID
	}
	rec := repository.NewAuditRecord(model.Scope{TenantID: msg.TenantID, FarmID: msg.FarmID}, runID,
		model.Actor{Type: model.ActorSystem}, model.AuditNotifySend, "order:"+strconv.FormatUint(msg.AggregateID, 10),
		msg.Payload, model.AuditResultOK, n.Clock.Now())
	if err := n.Audit.Append(ctx, rec); err != nil {
		return fmt.Errorf("service: notify.send outbox %d audit: %w", msg.ID, err)
	}
	n.Logger.InfoContext(ctx, "notify.send placeholder: logged, no channel until L5", "outbox_id", msg.ID,
		"kind", env.Kind, "order_id", env.OrderID, "farm_id", msg.FarmID, "dedup_key", msg.DedupKey)
	return nil
}
