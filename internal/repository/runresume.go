package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// L6：run 的续跑守卫与扫描任务用的 run 查询 / 迁移（P0 §6.7 流程 3-4）。

var _ agent.RunResumeStore = (*RunRepo)(nil)

// MaxSysRetry 是 SYSTEM_ERROR 可重入次数上限（P0 §6.7 功能任务 4），超过置 FAILED(RETRY_EXHAUSTED)。
const MaxSysRetry = 3

// MarkResumed 实现 agent.RunResumeStore（P0 §6.7 流程 3，文档只给步骤，SQL 按 FinishRun 的 row_ver 守卫写法）：
// run 须 INTERRUPTED 且 graph_version 一致；WAIT_APPROVAL 要求该 run 有 APPROVED 票，SYSTEM_ERROR 要求 sys_retry ≤ 3，
// WAIT_INPUT（P0 无补问入口）一律不可续。通过后同事务置 RUNNING、interrupt_type 清空、resume_epoch + 1，返回新 epoch。
func (r *RunRepo) MarkResumed(ctx context.Context, scope model.Scope, req agent.ResumeRequest) (int, error) {
	var epoch int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct {
			Status        string
			InterruptType *string
			GraphVersion  string
			ResumeEpoch   int
			SysRetry      int
			RowVer        int
		}
		res := tx.WithContext(ctx).Raw(
			`SELECT status, interrupt_type, graph_version, resume_epoch, sys_retry, row_ver FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
			req.RunID, scope.TenantID, scope.FarmID).Scan(&row)
		if res.Error != nil {
			return fmt.Errorf("repository: get agent_run %d: %w", req.RunID, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d: %w", req.RunID, model.ErrNotFound)
		}
		if model.RunStatus(row.Status) != model.RunInterrupted || row.InterruptType == nil || row.GraphVersion != req.GraphVersion {
			return fmt.Errorf("repository: run %d is %s graph_version %q (want %q): %w",
				req.RunID, row.Status, row.GraphVersion, req.GraphVersion, agent.ErrNotResumable)
		}
		if err := r.resumable(ctx, tx, scope, req.RunID, model.InterruptType(*row.InterruptType), row.SysRetry); err != nil {
			return err
		}
		upd := tx.WithContext(ctx).Exec(
			`UPDATE agent_run SET status='RUNNING', interrupt_type=NULL, resume_epoch=resume_epoch+1, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='INTERRUPTED' AND row_ver=?`,
			r.clock.Now(), req.RunID, scope.TenantID, scope.FarmID, row.RowVer)
		if upd.Error != nil {
			return fmt.Errorf("repository: resume agent_run %d: %w", req.RunID, upd.Error)
		}
		if upd.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d row_ver %d: %w", req.RunID, row.RowVer, model.ErrConcurrentUpdate)
		}
		epoch = row.ResumeEpoch + 1
		return nil
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// resumable 按中断类型校验续跑前置条件。
func (r *RunRepo) resumable(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64,
	it model.InterruptType, sysRetry int) error {
	switch it {
	case model.InterruptWaitApproval:
		var approved int64
		if err := tx.WithContext(ctx).Raw(
			`SELECT COUNT(*) FROM approval_ticket WHERE tenant_id=? AND farm_id=? AND run_id=? AND ticket_kind='ORDER' AND status='APPROVED'`,
			scope.TenantID, scope.FarmID, runID).Scan(&approved).Error; err != nil {
			return fmt.Errorf("repository: approved ticket of run %d: %w", runID, err)
		}
		if approved == 0 {
			return fmt.Errorf("repository: run %d has no APPROVED ticket: %w", runID, agent.ErrNotResumable)
		}
		return nil
	case model.InterruptSystemError:
		if sysRetry > MaxSysRetry {
			return fmt.Errorf("repository: run %d sys_retry %d: %w", runID, sysRetry, agent.ErrNotResumable)
		}
		return nil
	default:
		return fmt.Errorf("repository: run %d interrupt %s: %w", runID, it, agent.ErrNotResumable)
	}
}

// RunKey 是扫描任务取出的 run（跨农场，worker 专用）。
type RunKey struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	RowVer   int
	SysRetry int
}

// Scope 返回 run 所在农场的系统 Scope（无用户）。
func (k RunKey) Scope() model.Scope { return model.Scope{TenantID: k.TenantID, FarmID: k.FarmID} }

// scanLimit 是扫描任务单批上限（与文档扫描 SQL 的 LIMIT 100 一致）。
const scanLimit = 100

// ListStuck 照抄 P0 §6.7 卡住 run 扫描（idx_stuck）：RUNNING 且 updated_at 早于 before。
func (r *RunRepo) ListStuck(ctx context.Context, before time.Time) ([]RunKey, error) {
	var rows []RunKey
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, row_ver FROM agent_run WHERE status='RUNNING' AND updated_at < ? LIMIT 100`,
		before).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list stuck agent_run: %w", err)
	}
	return rows, nil
}

// MarkSystemError 照抄 P0 §6.7：RUNNING → INTERRUPTED(SYSTEM_ERROR)、sys_retry + 1；影响行数 0（已续期 / 已被改写）返回 false。
func (r *RunRepo) MarkSystemError(ctx context.Context, k RunKey, before time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		`UPDATE agent_run SET status='INTERRUPTED', interrupt_type='SYSTEM_ERROR', sys_retry=sys_retry+1, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING' AND updated_at < ? AND row_ver=?`,
		r.clock.Now(), k.ID, k.TenantID, k.FarmID, before, k.RowVer)
	if res.Error != nil {
		return false, fmt.Errorf("repository: mark agent_run %d SYSTEM_ERROR: %w", k.ID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// ListSystemError 取 INTERRUPTED(SYSTEM_ERROR) 的 run（文档未给 SQL；走 idx_stuck 的 status 前缀）。
func (r *RunRepo) ListSystemError(ctx context.Context) ([]RunKey, error) {
	var rows []RunKey
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, row_ver, sys_retry FROM agent_run WHERE status='INTERRUPTED' AND interrupt_type='SYSTEM_ERROR' LIMIT ?`,
		scanLimit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list SYSTEM_ERROR agent_run: %w", err)
	}
	return rows, nil
}

// FailInterruptedTx 在调用方事务内把 INTERRUPTED(it) 的 run 置 FAILED(reason)（row_ver 守卫，文档未给 SQL）；
// 冲突返回 model.ErrConcurrentUpdate。
func (r *RunRepo) FailInterruptedTx(ctx context.Context, tx *gorm.DB, k RunKey, it model.InterruptType,
	reason model.ReasonCode) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE agent_run SET status='FAILED', interrupt_type=NULL, reason_code=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='INTERRUPTED' AND interrupt_type=? AND row_ver=?`,
		string(reason), r.clock.Now(), k.ID, k.TenantID, k.FarmID, string(it), k.RowVer)
	if res.Error != nil {
		return fmt.Errorf("repository: fail agent_run %d: %w", k.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repository: agent_run %d row_ver %d: %w", k.ID, k.RowVer, model.ErrConcurrentUpdate)
	}
	return nil
}

// DraftOrdersTx 列出 run 名下仍为 DRAFT 的作业单（失败收尾时同事务 CANCELED）。
func (r *RunRepo) DraftOrdersTx(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64) ([]uint64, error) {
	var ids []uint64
	if err := tx.WithContext(ctx).Raw(
		`SELECT id FROM work_order WHERE tenant_id=? AND farm_id=? AND run_id=? AND status='DRAFT'`,
		scope.TenantID, scope.FarmID, runID).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: draft work_order of run %d: %w", runID, err)
	}
	return ids, nil
}

// RecheckEpoch 为下发前复检分配新 epoch（P0 §6.7 功能任务 3：复检以新 epoch 重取时效工具）：同事务 resume_epoch + 1 并读回，
// 每次复检的 epoch 都不同，时效工具的 call_key 随之变化，去重不会回放上一次复检的结果。只动计数，不改 status / row_ver。
func (r *RunRepo) RecheckEpoch(ctx context.Context, scope model.Scope, runID uint64) (int, error) {
	var epoch int
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Exec(`UPDATE agent_run SET resume_epoch=resume_epoch+1 WHERE id=? AND tenant_id=? AND farm_id=?`,
			runID, scope.TenantID, scope.FarmID)
		if res.Error != nil {
			return fmt.Errorf("repository: bump agent_run %d resume_epoch: %w", runID, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d: %w", runID, model.ErrNotFound)
		}
		if err := tx.Raw(`SELECT resume_epoch FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
			runID, scope.TenantID, scope.FarmID).Scan(&epoch).Error; err != nil {
			return fmt.Errorf("repository: agent_run %d resume_epoch: %w", runID, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}
