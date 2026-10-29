package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// DispatchRepo 是下发器（service.WorkOrderService.Dispatch）读写的作业单 / run / 约束校验 SQL，文档未给的另行标注。
// outbox 本身的 SQL 在 OutboxRepo。
type DispatchRepo struct {
	DB *gorm.DB
	OutboxRepo
}

// NewDispatchRepo 构造 DispatchRepo。
func NewDispatchRepo(db *gorm.DB) DispatchRepo {
	return DispatchRepo{DB: db, OutboxRepo: OutboxRepo{DB: db}}
}

// DispatchOrder 是下发器读取的作业单（文档未给 SQL）。
type DispatchOrder struct {
	ID                 uint64
	TenantID           uint64
	FarmID             uint64
	FieldID            uint64
	AdviceID           uint64
	RunID              uint64
	WorkType           string
	DeviceType         string
	Payload            json.RawMessage `gorm:"-"`
	TimeWindowFrom     time.Time
	TimeWindowTo       time.Time
	Status             string
	DispatchMode       string
	DispatchDeadlineAt *time.Time
	HasResult          bool
	RowVer             int
}

// Ref 返回作业单定位。
func (o DispatchOrder) Ref() model.OrderRef {
	return model.OrderRef{TenantID: o.TenantID, FarmID: o.FarmID, OrderID: o.ID}
}

// WorkOrder 转为 Adapter 入参。
func (o DispatchOrder) WorkOrder() model.WorkOrder {
	return model.WorkOrder{OrderID: o.ID, AdviceID: o.AdviceID, RunID: o.RunID, TenantID: o.TenantID, FarmID: o.FarmID,
		FieldID: o.FieldID, WorkType: model.WorkType(o.WorkType), DeviceType: o.DeviceType, Payload: o.Payload,
		TimeWindow: model.TimeWindow{From: o.TimeWindowFrom, To: o.TimeWindowTo},
		Status:     model.WorkOrderStatus(o.Status), DispatchMode: model.DispatchMode(o.DispatchMode)}
}

// OrderForDispatch 读作业单的下发所需列；db 为 nil 时用连接。
func (r DispatchRepo) OrderForDispatch(ctx context.Context, db *gorm.DB, ref model.OrderRef) (DispatchOrder, error) {
	if db == nil {
		db = r.DB
	}
	var row struct {
		DispatchOrder
		PayloadRaw []byte `gorm:"column:payload"`
	}
	res := db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload, time_window_from, time_window_to,
  status, dispatch_mode, dispatch_deadline_at, dispatch_result IS NOT NULL AS has_result, row_ver
 FROM work_order WHERE id=? AND tenant_id=? AND farm_id=?`,
		ref.OrderID, ref.TenantID, ref.FarmID).Scan(&row)
	if res.Error != nil {
		return DispatchOrder{}, fmt.Errorf("repository: dispatch read work_order %d: %w", ref.OrderID, res.Error)
	}
	if res.RowsAffected == 0 {
		return DispatchOrder{}, fmt.Errorf("repository: work_order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	o := row.DispatchOrder
	o.Payload = rawJSON(row.PayloadRaw)
	return o, nil
}

// RecheckSource 是复检重建原问题所需的 run 信息：提问人、地块与 RUN_STARTED 中（已脱敏）的问题文本。
type RecheckSource struct {
	UserID   uint64
	FieldID  uint64
	Question string
}

// RecheckSource 读原 run 的提问人 / 地块与 RUN_STARTED 问题（文档未给 SQL；原问题不另存，只能取脱敏后的文本）。
func (r DispatchRepo) RecheckSource(ctx context.Context, scope model.Scope, runID uint64) (RecheckSource, error) {
	var run struct {
		UserID  uint64
		FieldID uint64
	}
	res := r.DB.WithContext(ctx).Raw(`SELECT user_id, field_id FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
		runID, scope.TenantID, scope.FarmID).Scan(&run)
	if res.Error != nil {
		return RecheckSource{}, fmt.Errorf("repository: recheck source run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return RecheckSource{}, fmt.Errorf("repository: agent_run %d: %w", runID, model.ErrNotFound)
	}
	var payload []byte
	res = r.DB.WithContext(ctx).Raw(
		`SELECT payload FROM run_event WHERE tenant_id=? AND farm_id=? AND run_id=? AND event_type=? ORDER BY seq LIMIT 1`,
		scope.TenantID, scope.FarmID, runID, string(model.EventRunStarted)).Scan(&payload)
	if res.Error != nil {
		return RecheckSource{}, fmt.Errorf("repository: recheck source run %d question: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return RecheckSource{}, fmt.Errorf("repository: run %d RUN_STARTED: %w", runID, model.ErrNotFound)
	}
	var q model.RunStartedPayload
	if err := json.Unmarshal(payload, &q); err != nil {
		return RecheckSource{}, fmt.Errorf("repository: run %d RUN_STARTED payload: %w", runID, err)
	}
	return RecheckSource{UserID: run.UserID, FieldID: run.FieldID, Question: q.Question}, nil
}

// SupersedeRun 写旧 run 的 superseded_by_run_id（复检结论变化、新建 run 后；文档未给 SQL）。
// 只补空值、不动 status / row_ver（旧 run 可能仍在 run_finalize 收尾，避免与其乐观锁冲突）；已写过返回 false。
func (r DispatchRepo) SupersedeRun(ctx context.Context, scope model.Scope, oldRunID, newRunID uint64, now time.Time) (bool, error) {
	res := r.DB.WithContext(ctx).Exec(
		`UPDATE agent_run SET superseded_by_run_id=?, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND superseded_by_run_id IS NULL`,
		newRunID, now, oldRunID, scope.TenantID, scope.FarmID)
	if res.Error != nil {
		return false, fmt.Errorf("repository: supersede agent_run %d: %w", oldRunID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// InsertPreDispatchChecks 写下发前约束校验（与 §6.6 审批前同一 INSERT IGNORE，uk_check 幂等）；attempt 取复检 epoch。
func (r DispatchRepo) InsertPreDispatchChecks(ctx context.Context, ref model.OrderRef, attempt int,
	checks []model.ConstraintCheck, now time.Time) error {
	for _, c := range checks {
		if c.Phase != model.PhasePreDispatch {
			return fmt.Errorf("repository: constraint_check %s phase %q, want %s", c.RuleID, c.Phase, model.PhasePreDispatch)
		}
	}
	return inTx(ctx, r.DB, func(tx *gorm.DB) error {
		for _, c := range checks {
			if err := tx.Exec(
				`INSERT IGNORE INTO constraint_check (tenant_id, farm_id, order_id, phase, attempt, rule_id, passed, observed, threshold, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ref.TenantID, ref.FarmID, ref.OrderID, string(c.Phase), attempt, c.RuleID, c.Passed,
				string(c.Observed), string(c.Threshold), now).Error; err != nil {
				return fmt.Errorf("repository: insert constraint_check %s order %d rule %s: %w", c.Phase, ref.OrderID, c.RuleID, err)
			}
		}
		return nil
	})
}
