package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ErrDuplicateAttempt 表示 dispatch_attempt 唯一键冲突（uk_order_attempt / uk_idem / uk_job）：同一次尝试已写过。
var ErrDuplicateAttempt = errors.New("repository: duplicate dispatch_attempt")

// mysqlDuplicateKey 是 ER_DUP_ENTRY。
const mysqlDuplicateKey = 1062

// attemptCols 是 dispatch_attempt 读列（与 attemptRow 字段一一对应）。
const attemptCols = `id, tenant_id, farm_id, order_id, ticket_id, attempt, device_type, device_id, idem_key, device_job_id, mode, status,
 failure_kind, done_area_mu, command_raw_id, flag_read_at, sent_at, receipt_due_at, row_ver, updated_at`

// DispatchAttemptRepo 读写 dispatch_attempt（0007，P1 §6.3）。业务方法按 (tenant_id, farm_id) 过滤；
// 回执反查 Scope（uk_job）无用户身份，只经 SystemScope。本层不接下发器（L4 接入）。
type DispatchAttemptRepo struct{ DB *gorm.DB }

// InsertSentTx 在调用 Adapter 前写一条 SENT 尝试（row_ver=0、updated_at=sent_at），返回 id。
// 唯一键冲突（同单同 attempt / 同 idem_key）→ ErrDuplicateAttempt。
func (r DispatchAttemptRepo) InsertSentTx(ctx context.Context, tx *gorm.DB, a model.DispatchAttempt) (uint64, error) {
	if err := requireFarm(a.TenantID, a.FarmID); err != nil {
		return 0, err
	}
	res, err := execInTx(ctx, tx,
		`INSERT INTO dispatch_attempt (tenant_id, farm_id, order_id, ticket_id, attempt, device_type, device_id, idem_key, mode, status, command_raw_id, flag_read_at, sent_at, receipt_due_at, row_ver, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		a.TenantID, a.FarmID, a.OrderID, a.TicketID, a.Attempt, a.DeviceType, a.DeviceID, a.IdemKey, string(a.Mode),
		string(model.AttemptSent), a.CommandRawID, a.FlagReadAt, a.SentAt, a.ReceiptDueAt, a.SentAt)
	if err != nil {
		return 0, fmt.Errorf("repository: insert dispatch_attempt order %d attempt %d: %w", a.OrderID, a.Attempt, attemptWriteErr(err))
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("repository: dispatch_attempt id: %w", err)
	}
	return uint64(id), nil
}

// SetJobTx 记下平台受理结果：写 device_job_id 与回执状态、推新的 receipt_due_at（row_ver 守卫）。
// 入口先校验回执（DeviceReceipt.Validate，不合规 → model.ErrBadReceipt，不写库）；DoneAreaMu 为 nil 时保留旧值。
// 影响行数 0 → model.ErrConcurrentUpdate；同 device_type 下 job 重复 → ErrDuplicateAttempt。
func (r DispatchAttemptRepo) SetJobTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, id uint64, rowVer int,
	rc model.DeviceReceipt, receiptDueAt, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if rc.DeviceJobID == "" {
		return fmt.Errorf("repository: set dispatch_attempt %d job: empty device_job_id", id)
	}
	if err := rc.Validate(); err != nil {
		return fmt.Errorf("repository: set dispatch_attempt %d job: %w", id, err)
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE dispatch_attempt SET device_job_id=?, status=?, failure_kind=?, done_area_mu=COALESCE(?, done_area_mu), receipt_due_at=?, row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND row_ver=?`,
		rc.DeviceJobID, string(rc.Status), nullKind(rc.FailureKind), rc.DoneAreaMu, receiptDueAt, now,
		ref.TenantID, ref.FarmID, id, rowVer)
	if res.Error != nil {
		res.Error = attemptWriteErr(res.Error)
	}
	return affected(res, fmt.Sprintf("set dispatch_attempt %d job", id))
}

// UpdateReceiptTx 照抄 P1 §6.3 回执 UPDATE（补 failure_kind 列）：按 row_ver 乐观锁写状态 / 完成面积 / 新 receipt_due_at。
// 入口先校验 model.CheckAttemptState（status 已登记且非 SENT、FAILED 必带 kind、面积 >= 0），不合规 → model.ErrBadReceipt，
// 不写库；doneAreaMu 为 nil 时保留旧值（COALESCE），避免无面积的后续回执抹掉已记的部分作业面积。
// 影响行数 0 → model.ErrConcurrentUpdate（调用方重读后按状态机决定是否重试）。
func (r DispatchAttemptRepo) UpdateReceiptTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, id uint64, rowVer int,
	status model.AttemptStatus, kind model.FailureKind, doneAreaMu *float64, receiptDueAt, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if status == model.AttemptSent {
		return fmt.Errorf("repository: update dispatch_attempt %d receipt: %w: status SENT is insert-only", id, model.ErrBadReceipt)
	}
	if err := model.CheckAttemptState(status, kind, doneAreaMu); err != nil {
		return fmt.Errorf("repository: update dispatch_attempt %d receipt: %w", id, err)
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE dispatch_attempt SET status=?, failure_kind=?, done_area_mu=COALESCE(?, done_area_mu), receipt_due_at=?, row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND row_ver=?`,
		string(status), nullKind(kind), doneAreaMu, receiptDueAt, now, ref.TenantID, ref.FarmID, id, rowVer)
	return affected(res, fmt.Sprintf("update dispatch_attempt %d receipt", id))
}

// RefreshSentTx 是续跑重发前的写（P1-L8-B 审查 P0-2）：SENT 且无 device_job_id 的尝试刷新 flag_read_at / sent_at /
// receipt_due_at（row_ver 守卫），让关停报告按本次重发的读开关与下发时刻核对。影响行数 0 → model.ErrConcurrentUpdate。
func (r DispatchAttemptRepo) RefreshSentTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, id uint64, rowVer int,
	flagReadAt, sentAt, receiptDueAt time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if flagReadAt.After(sentAt) {
		return fmt.Errorf("repository: refresh dispatch_attempt %d: flag_read_at after sent_at", id)
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE dispatch_attempt SET flag_read_at=?, sent_at=?, receipt_due_at=?, row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND row_ver=? AND status='SENT' AND device_job_id IS NULL`,
		flagReadAt, sentAt, receiptDueAt, sentAt, ref.TenantID, ref.FarmID, id, rowVer)
	return affected(res, fmt.Sprintf("refresh dispatch_attempt %d sent", id))
}

// ByOrder 列出作业单的全部尝试（attempt 升序），供下一次 attempt 编号与对账页使用。
func (r DispatchAttemptRepo) ByOrder(ctx context.Context, ref model.OrderRef) ([]model.DispatchAttempt, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return nil, err
	}
	var rows []attemptRow
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT `+attemptCols+` FROM dispatch_attempt WHERE tenant_id=? AND farm_id=? AND order_id=? ORDER BY attempt`,
		ref.TenantID, ref.FarmID, ref.OrderID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list dispatch_attempt order %d: %w", ref.OrderID, err)
	}
	return attempts(rows), nil
}

// PendingReconcile 按 Scope 列待人工对账的尝试（回执映射表中 Effect=RECONCILE：NO_RECEIPT、FAILED+UNKNOWN），
// 按 sent_at 升序，走 idx_scope_status。
func (r DispatchAttemptRepo) PendingReconcile(ctx context.Context, scope model.Scope, limit int) ([]model.DispatchAttempt, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("repository: pending reconcile limit %d must be positive", limit)
	}
	var rows []attemptRow
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT `+attemptCols+` FROM dispatch_attempt WHERE tenant_id=? AND farm_id=? AND (status='NO_RECEIPT' OR (status='FAILED' AND failure_kind='UNKNOWN')) ORDER BY sent_at, id LIMIT ?`,
		scope.TenantID, scope.FarmID, limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: pending reconcile (%d,%d): %w", scope.TenantID, scope.FarmID, err)
	}
	return attempts(rows), nil
}

// AttemptOwner 是按平台任务号反查到的尝试归属（P1 §6.3 uk_job 反查 SQL 的列）。
type AttemptOwner struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	OrderID  uint64
	Attempt  int
	Status   model.AttemptStatus
}

// ByJob 照抄 P1 §6.3 反查 SQL：回执只带 (device_type, device_job_id)，按 uk_job 找回 Scope；须持 SystemScope。
// 找不到 → model.ErrNotFound。之后的读写须用返回的 (tenant_id, farm_id) 走业务方法。
func (r DispatchAttemptRepo) ByJob(ctx context.Context, sys SystemScope, deviceType, jobID string) (AttemptOwner, error) {
	if err := requireSystem(sys); err != nil {
		return AttemptOwner{}, err
	}
	var row AttemptOwner
	res := r.DB.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, order_id, attempt, status FROM dispatch_attempt WHERE device_type=? AND device_job_id=?`,
		deviceType, jobID).Scan(&row)
	if res.Error != nil {
		return AttemptOwner{}, fmt.Errorf("repository: dispatch_attempt by job %s/%s (%s): %w", deviceType, jobID, sys.Task(), res.Error)
	}
	if res.RowsAffected == 0 {
		return AttemptOwner{}, fmt.Errorf("repository: dispatch_attempt by job %s/%s: %w", deviceType, jobID, model.ErrNotFound)
	}
	return row, nil
}

// attemptWriteErr 把唯一键冲突映射为 ErrDuplicateAttempt，其余原样返回。
func attemptWriteErr(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == mysqlDuplicateKey {
		return fmt.Errorf("%w: %w", ErrDuplicateAttempt, err)
	}
	return err
}

// nullKind 把空 FailureKind 写成 NULL。
func nullKind(k model.FailureKind) sql.NullString {
	return sql.NullString{String: string(k), Valid: k != ""}
}

// attemptRow 是 attemptCols 的扫描行（可空列用 Null 类型）。
type attemptRow struct {
	ID           uint64
	TenantID     uint64
	FarmID       uint64
	OrderID      uint64
	TicketID     uint64
	Attempt      int
	DeviceType   string
	DeviceID     string
	IdemKey      string
	DeviceJobID  sql.NullString
	Mode         string
	Status       string
	FailureKind  sql.NullString
	DoneAreaMu   sql.NullFloat64
	CommandRawID uint64
	FlagReadAt   time.Time
	SentAt       time.Time
	ReceiptDueAt time.Time
	RowVer       int
	UpdatedAt    time.Time
}

func attempts(rows []attemptRow) []model.DispatchAttempt {
	out := make([]model.DispatchAttempt, 0, len(rows))
	for _, w := range rows {
		a := model.DispatchAttempt{ID: w.ID, TenantID: w.TenantID, FarmID: w.FarmID, OrderID: w.OrderID, TicketID: w.TicketID,
			Attempt: w.Attempt, DeviceType: w.DeviceType, DeviceID: w.DeviceID, IdemKey: w.IdemKey, DeviceJobID: w.DeviceJobID.String,
			Mode: model.DispatchMode(w.Mode), Status: model.AttemptStatus(w.Status), FailureKind: model.FailureKind(w.FailureKind.String),
			CommandRawID: w.CommandRawID, FlagReadAt: w.FlagReadAt, SentAt: w.SentAt, ReceiptDueAt: w.ReceiptDueAt,
			RowVer: w.RowVer, UpdatedAt: w.UpdatedAt}
		if w.DoneAreaMu.Valid {
			v := w.DoneAreaMu.Float64
			a.DoneAreaMu = &v
		}
		out = append(out, a)
	}
	return out
}
