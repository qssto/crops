package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// ErrBadDraft 表示作业单草稿参数不合法。
var ErrBadDraft = errors.New("repository: bad work order draft")

// OrderDraftRepo 实现 agent.OrderDraftStore：同一事务写 work_order(DRAFT, DRY_RUN) + work_order_transition
// + run_event(ORDER_TRANSITION)；uk_advice 保证一卡一单，重跑返回已有 order_id 且不重复写迁移与事件。
type OrderDraftRepo struct {
	db     *gorm.DB
	clock  clock.Clock
	orders WorkOrderRepo
	events EventRepo
}

var _ agent.OrderDraftStore = (*OrderDraftRepo)(nil)

// NewOrderDraftRepo 构造 OrderDraftRepo。
func NewOrderDraftRepo(db *gorm.DB, c clock.Clock) *OrderDraftRepo {
	return &OrderDraftRepo{db: db, clock: c}
}

// draftPayload 是建单 ORDER_TRANSITION 事件的 payload（与 service.Transition 同形）。
type draftPayload struct {
	OrderID uint64                `json:"order_id"`
	From    model.WorkOrderStatus `json:"from"`
	To      model.WorkOrderStatus `json:"to"`
}

func validateDraft(scope model.Scope, o model.WorkOrder) error {
	if err := checkScope(scope, o.TenantID, o.FarmID); err != nil {
		return err
	}
	var problems []string
	if o.AdviceID == 0 || o.RunID == 0 || o.FieldID == 0 {
		problems = append(problems, "advice_id / run_id / field_id required")
	}
	if o.Status != "" && o.Status != model.OrderDraft {
		problems = append(problems, fmt.Sprintf("status %s, want DRAFT", o.Status))
	}
	if o.DispatchMode != "" && o.DispatchMode != model.DispatchDryRun {
		problems = append(problems, fmt.Sprintf("dispatch_mode %s, P0 fixed DRY_RUN", o.DispatchMode))
	}
	if o.TimeWindow.From.IsZero() || o.TimeWindow.To.Before(o.TimeWindow.From) {
		problems = append(problems, "time_window invalid")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %v", ErrBadDraft, problems)
	}
	return nil
}

// CreateDraft 建 DRAFT 单；首条迁移 from_status 写空串（列 NOT NULL，表示「无 → DRAFT」）。
func (r *OrderDraftRepo) CreateDraft(ctx context.Context, scope model.Scope, o model.WorkOrder) (uint64, error) {
	if err := validateDraft(scope, o); err != nil {
		return 0, err
	}
	payload := o.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	now := r.clock.Now()
	var orderID uint64
	err := inTx(ctx, r.db, func(tx *gorm.DB) error {
		res, err := execInTx(ctx, tx,
			`INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload, time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
			scope.TenantID, scope.FarmID, o.FieldID, o.AdviceID, o.RunID, string(o.WorkType), o.DeviceType,
			string(payload), o.TimeWindow.From, o.TimeWindow.To, string(model.OrderDraft),
			string(model.DispatchDryRun), model.ContractVersion, now, now)
		if err != nil {
			return fmt.Errorf("repository: insert work_order advice %d: %w", o.AdviceID, err)
		}
		last, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: work_order id: %w", err)
		}
		orderID = uint64(last)
		// 新插入影响 1 行；命中 uk_advice 且 id 未变时为 0 行（未开 CLIENT_FOUND_ROWS），已有单不再写迁移。
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("repository: work_order rows: %w", err)
		}
		if n != 1 {
			return nil
		}
		return r.recordCreated(ctx, tx, scope, o.RunID, orderID, now)
	})
	if err != nil {
		return 0, err
	}
	return orderID, nil
}

func (r *OrderDraftRepo) recordCreated(ctx context.Context, tx *gorm.DB, scope model.Scope, runID, orderID uint64, now time.Time) error {
	ref := model.OrderRef{TenantID: scope.TenantID, FarmID: scope.FarmID, OrderID: orderID}
	actor := model.Actor{Type: model.ActorAgent}
	if err := r.orders.InsertTransition(ctx, tx, model.OrderTransition{
		Ref: ref, RunID: runID, From: "", To: model.OrderDraft, Actor: actor, CreatedAt: now,
	}); err != nil {
		return err
	}
	body, err := json.Marshal(draftPayload{OrderID: orderID, To: model.OrderDraft})
	if err != nil {
		return fmt.Errorf("repository: encode draft payload: %w", err)
	}
	if _, err := r.events.AppendTx(ctx, tx, model.RunEvent{TenantID: scope.TenantID, FarmID: scope.FarmID,
		RunID: runID, Type: model.EventOrderTransition, Actor: actor.Type, Payload: body, CreatedAt: now}); err != nil {
		return fmt.Errorf("repository: draft order %d event: %w", orderID, err)
	}
	return nil
}
