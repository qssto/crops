package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L5 即将过期提醒（§6.6 流程 5）：worker 扫描 expires_at ∈ [now, now+1h] 的 OPEN 票，逐张写 notify.send(EXPIRING_SOON)；
// dedup_key = notify:EXPIRING_SOON:{order_id}:{ticket_id}，INSERT IGNORE 保证每张票只提醒一次（多轮 / 多实例命中同一张票不重发）。

// ScanApprovalExpiring 是即将过期提醒的任务名（lock:scan:approval_expiring，TTL 60s）。
const ScanApprovalExpiring = "approval_expiring"

// ExpiringScanStore 是即将过期扫描的仓储能力，repository.DecisionRepo 实现。
type ExpiringScanStore interface {
	InTx(ctx context.Context, fn func(tx *repository.DB) error) error
	ListExpiring(ctx context.Context, sys repository.SystemScope, now time.Time) ([]repository.ExpiringTicket, error)
}

// approvalExpiring 逐张写提醒；单张失败不影响其余，错误汇总返回（下一轮重试，已写的靠 dedup_key 去重）。
func (s *Scanner) approvalExpiring(ctx context.Context, now time.Time) error {
	rows, err := s.d.Expiring.ListExpiring(ctx, scanSystem, now)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range rows {
		if err := s.notifyExpiring(ctx, t, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Scanner) notifyExpiring(ctx context.Context, t repository.ExpiringTicket, now time.Time) error {
	body, err := json.Marshal(model.NotifyEnvelope{Kind: model.NotifyExpiringSoon, OrderID: t.OrderID, TicketID: t.ID})
	if err != nil {
		return fmt.Errorf("encode expiring ticket %d: %w", t.ID, err)
	}
	ref := model.OrderRef{TenantID: t.TenantID, FarmID: t.FarmID, OrderID: t.OrderID}
	key := model.NotifyDedupKey(model.NotifyExpiringSoon, t.OrderID, t.ID)
	return s.d.Expiring.InTx(ctx, func(tx *repository.DB) error {
		if _, err := s.d.Notify.InsertTx(ctx, tx, ref, key, body, now); err != nil {
			return fmt.Errorf("notify expiring ticket %d: %w", t.ID, err)
		}
		return nil
	})
}
