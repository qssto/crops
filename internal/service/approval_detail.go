package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L5 待审批详情（§6.6 功能任务 4）：票据 + 完整快照（命中原因、高亮、差异）+ PRE_APPROVAL 与最新一轮 PRE_DISPATCH 校验。

// ApprovalDetailStore 是详情需要的票据与作业单读取，*repository.DecisionRepo 实现。
type ApprovalDetailStore interface {
	TicketTx(ctx context.Context, db *repository.DB, scope model.Scope, ticketID uint64) (repository.TicketRow, error)
	OrdersWithChecks(ctx context.Context, scope model.Scope, ids []uint64) (map[uint64]model.WorkOrder, error)
}

// SnapshotReader 按 raw_object id 读回快照本体，repository.RawReader 实现。
type SnapshotReader interface {
	Get(ctx context.Context, scope model.Scope, rawObjectID uint64) ([]byte, error)
}

// ErrSnapshotCorrupt：raw_store 中的快照与票据 snapshot_digest 不符（数据完整性错误，→ 500）。
var ErrSnapshotCorrupt = errors.New("SNAPSHOT_CORRUPT")

// ApprovalDetail 是 GET /approvals/:ticket_id 的响应。Snapshot 为开票时冻结的完整快照（digest 校验过）；
// RiskReasons / Highlights / Diff 取自快照，便于前端直接绑定。
type ApprovalDetail struct {
	Ticket            model.ApprovalTicket    `json:"ticket"`
	AwaitingConfirm   bool                    `json:"awaiting_confirm"`
	WorkOrder         model.WorkOrder         `json:"work_order"`
	Snapshot          json.RawMessage         `json:"snapshot"`
	RiskReasons       []string                `json:"risk_reasons"`
	Highlights        *OrderHighlights        `json:"highlights"`
	Diff              *OrderDiff              `json:"diff"`
	PreApprovalChecks []model.ConstraintCheck `json:"pre_approval_checks"`
	PreDispatchChecks []model.ConstraintCheck `json:"pre_dispatch_checks"`
}

// ApprovalDetailService 组装待审批详情（只读）。
type ApprovalDetailService struct {
	store ApprovalDetailStore
	raw   SnapshotReader
}

// NewApprovalDetailService 校验依赖。
func NewApprovalDetailService(store ApprovalDetailStore, raw SnapshotReader) (*ApprovalDetailService, error) {
	if store == nil || raw == nil {
		return nil, errors.New("service: approval detail needs Store and SnapshotReader")
	}
	return &ApprovalDetailService{store: store, raw: raw}, nil
}

// Detail 读本农场一张 ORDER 票的详情。票据未命中（不存在或属他场）→ ErrNotInScope；作业单 / 快照缺失或快照 digest
// 不符是数据完整性错误（不含 model.ErrNotFound，→ 500）。
func (s *ApprovalDetailService) Detail(ctx context.Context, scope model.Scope, ticketID uint64) (ApprovalDetail, error) {
	if ticketID == 0 {
		return ApprovalDetail{}, fmt.Errorf("%w: ticket_id required", ErrBadInput)
	}
	row, err := s.store.TicketTx(ctx, nil, scope, ticketID)
	if err != nil {
		return ApprovalDetail{}, fmt.Errorf("service: approval detail: %w", notInScope(err))
	}
	t, err := row.Ticket()
	if err != nil {
		return ApprovalDetail{}, fmt.Errorf("service: approval detail ticket %d: %w", ticketID, err)
	}
	orders, err := s.store.OrdersWithChecks(ctx, scope, []uint64{row.OrderID})
	if err != nil {
		return ApprovalDetail{}, fmt.Errorf("service: approval detail ticket %d order: %w", ticketID, err)
	}
	o, ok := orders[row.OrderID]
	if !ok {
		return ApprovalDetail{}, fmt.Errorf("service: approval detail ticket %d: work order %d missing: data integrity",
			ticketID, row.OrderID)
	}
	body, snap, err := s.snapshot(ctx, scope, row)
	if err != nil {
		return ApprovalDetail{}, err
	}
	o.ApprovalExpiresAt = row.ExpiresAt
	pre, dispatch := splitChecks(o.ConstraintChecks)
	reasons := snap.RiskReasons
	if reasons == nil {
		reasons = []string{}
	}
	return ApprovalDetail{Ticket: t, AwaitingConfirm: needsConfirm(row) && row.FirstApproverID != nil, WorkOrder: o,
		Snapshot: body, RiskReasons: reasons, Highlights: snap.Highlights, Diff: snap.Diff,
		PreApprovalChecks: pre, PreDispatchChecks: dispatch}, nil
}

// snapshot 读回快照并核对 digest（与开票时 sha256(body) 一致）；读失败一律按完整性错误处理（剥掉 ErrNotFound）。
func (s *ApprovalDetailService) snapshot(ctx context.Context, scope model.Scope, row repository.TicketRow) (json.RawMessage,
	approvalSnapshot, error) {
	body, err := s.raw.Get(ctx, scope, row.SnapshotRawID)
	if err != nil {
		return nil, approvalSnapshot{}, fmt.Errorf("service: approval detail ticket %d snapshot %d: %v: data integrity",
			row.ID, row.SnapshotRawID, err)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != row.SnapshotDigest {
		return nil, approvalSnapshot{}, fmt.Errorf("service: approval detail ticket %d: %w", row.ID, ErrSnapshotCorrupt)
	}
	var snap approvalSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return nil, approvalSnapshot{}, fmt.Errorf("service: approval detail ticket %d decode snapshot: %w", row.ID, err)
	}
	return body, snap, nil
}

// splitChecks 取全部 PRE_APPROVAL 校验与最新一轮（attempt 最大）的 PRE_DISPATCH 校验；两者都不为 nil。
func splitChecks(cs []model.ConstraintCheck) (pre, dispatch []model.ConstraintCheck) {
	pre, dispatch = []model.ConstraintCheck{}, []model.ConstraintCheck{}
	latest := 0
	for _, c := range cs {
		if c.Phase == model.PhasePreDispatch && c.Attempt > latest {
			latest = c.Attempt
		}
	}
	for _, c := range cs {
		switch {
		case c.Phase == model.PhasePreApproval:
			pre = append(pre, c)
		case c.Phase == model.PhasePreDispatch && c.Attempt == latest:
			dispatch = append(dispatch, c)
		}
	}
	return pre, dispatch
}
