package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// RunRepo 读写 agent_run / run_step，实现 agent.RunStateStore；每个方法自管事务，时间取注入的 Clock。
type RunRepo struct {
	db    *gorm.DB
	clock clock.Clock
}

var _ agent.RunStateStore = (*RunRepo)(nil)

// NewRunRepo 构造 RunRepo。
func NewRunRepo(db *gorm.DB, c clock.Clock) *RunRepo { return &RunRepo{db: db, clock: c} }

// CreateRun 插入 agent_run(RUNNING)，返回 run_id。s 只取 TenantID / FarmID。
func (r *RunRepo) CreateRun(ctx context.Context, s model.RunState, userID, fieldID uint64, meta agent.RunMeta) (uint64, error) {
	now := r.clock.Now()
	var id uint64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res, err := execInTx(ctx, tx,
			`INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version, model_tier, knowledge_ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.TenantID, s.FarmID, fieldID, userID, string(model.RunRunning),
			meta.GraphVersion, meta.PromptVersion, meta.ModelTier, meta.KnowledgeRef, now, now)
		if err != nil {
			return fmt.Errorf("repository: insert agent_run: %w", err)
		}
		last, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: agent_run id: %w", err)
		}
		id = uint64(last)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// runRow 是 agent_run 的读取列。
type runRow struct {
	ID         uint64
	TenantID   uint64
	FarmID     uint64
	FieldID    uint64
	UserID     uint64
	Status     string
	ReasonCode *string
	RowVer     int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (r *RunRepo) getRun(ctx context.Context, db *gorm.DB, scope model.Scope, runID uint64) (runRow, error) {
	var row runRow
	res := db.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, field_id, user_id, status, reason_code, row_ver, created_at, updated_at FROM agent_run WHERE id=? AND tenant_id=? AND farm_id=?`,
		runID, scope.TenantID, scope.FarmID).Scan(&row)
	if res.Error != nil {
		return runRow{}, fmt.Errorf("repository: get agent_run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return runRow{}, fmt.Errorf("repository: agent_run %d: %w", runID, model.ErrNotFound)
	}
	return row, nil
}

// Summary 返回 run 状态视图（GET /runs/{run_id}）。
func (r *RunRepo) Summary(ctx context.Context, scope model.Scope, runID uint64) (model.RunSummary, error) {
	row, err := r.getRun(ctx, r.db, scope, runID)
	if err != nil {
		return model.RunSummary{}, err
	}
	s := model.RunSummary{RunID: row.ID, FieldID: row.FieldID, UserID: row.UserID,
		Status: model.RunStatus(row.Status), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.ReasonCode != nil {
		s.ReasonCode = *row.ReasonCode
	}
	return s, nil
}

// LoadRun 读 agent_run 与全部 run_step（按 id 序）；CompletedStepIDs 取 COMPLETED 的步。
func (r *RunRepo) LoadRun(ctx context.Context, scope model.Scope, runID uint64) (model.RunState, error) {
	row, err := r.getRun(ctx, r.db, scope, runID)
	if err != nil {
		return model.RunState{}, err
	}
	var steps []struct {
		StepID    string
		Node      string
		Status    string
		StartedAt time.Time
		EndedAt   *time.Time
		ErrorCode *string
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT step_id, node, status, started_at, ended_at, error_code FROM run_step WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`,
		scope.TenantID, scope.FarmID, runID).Scan(&steps).Error; err != nil {
		return model.RunState{}, fmt.Errorf("repository: list run_step %d: %w", runID, err)
	}
	st := model.RunState{RunID: row.ID, TenantID: row.TenantID, FarmID: row.FarmID, CreatedAt: row.CreatedAt,
		Steps: make([]model.RunStep, 0, len(steps)), CompletedStepIDs: []string{}}
	for _, s := range steps {
		step := model.RunStep{StepID: s.StepID, Node: s.Node, Status: model.StepStatus(s.Status),
			StartedAt: s.StartedAt, EndedAt: s.EndedAt}
		if s.ErrorCode != nil {
			step.ErrorCode = *s.ErrorCode
		}
		st.Steps = append(st.Steps, step)
		if step.Status == model.StepCompleted {
			st.CompletedStepIDs = append(st.CompletedStepIDs, step.StepID)
		}
	}
	return st, nil
}

// SaveStep 照抄 P0 L4 run_step 行别名 upsert：开始传 STARTED，结束传终态与原 started_at。
func (r *RunRepo) SaveStep(ctx context.Context, scope model.Scope, runID uint64, step model.RunStep) error {
	err := r.db.WithContext(ctx).Exec(
		`INSERT INTO run_step (tenant_id, farm_id, run_id, step_id, node, status, started_at, ended_at, error_code)
VALUES (?,?,?,?,?,?,?,?,?) AS new
ON DUPLICATE KEY UPDATE status=new.status, ended_at=new.ended_at, error_code=new.error_code, row_ver=run_step.row_ver+1`,
		scope.TenantID, scope.FarmID, runID, step.StepID, step.Node, string(step.Status),
		step.StartedAt, step.EndedAt, nullIfEmpty(step.ErrorCode)).Error
	if err != nil {
		return fmt.Errorf("repository: upsert run_step %d/%s: %w", runID, step.StepID, err)
	}
	return nil
}

// FinishRun 用 row_ver 乐观锁把 RUNNING 置为终态：非终态目标或当前非 RUNNING → model.ErrIllegalTransition，
// 并发改写 → model.ErrConcurrentUpdate。
func (r *RunRepo) FinishRun(ctx context.Context, scope model.Scope, runID uint64, status model.RunStatus, reasonCode string) error {
	if !status.IsTerminal() {
		return fmt.Errorf("repository: finish run %d to %s: %w", runID, status, model.ErrIllegalTransition)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := r.getRun(ctx, tx, scope, runID)
		if err != nil {
			return err
		}
		if model.RunStatus(row.Status) != model.RunRunning {
			return fmt.Errorf("repository: finish run %d %s -> %s: %w", runID, row.Status, status, model.ErrIllegalTransition)
		}
		res := tx.WithContext(ctx).Exec(
			`UPDATE agent_run SET status=?, reason_code=?, row_ver=row_ver+1, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING' AND row_ver=?`,
			string(status), nullIfEmpty(reasonCode), r.clock.Now(), runID, scope.TenantID, scope.FarmID, row.RowVer)
		if res.Error != nil {
			return fmt.Errorf("repository: finish agent_run %d: %w", runID, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("repository: agent_run %d row_ver %d: %w", runID, row.RowVer, model.ErrConcurrentUpdate)
		}
		return nil
	})
}

// Heartbeat 照抄 P0 L6 心跳 SQL（不改状态、不升 row_ver）。
// 影响行数 0 不报错：FrozenClock 下 updated_at 不变时 MySQL 也返回 0 行。
func (r *RunRepo) Heartbeat(ctx context.Context, scope model.Scope, runID uint64) error {
	err := r.db.WithContext(ctx).Exec(
		`UPDATE agent_run SET updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status='RUNNING'`,
		r.clock.Now(), runID, scope.TenantID, scope.FarmID).Error
	if err != nil {
		return fmt.Errorf("repository: heartbeat agent_run %d: %w", runID, err)
	}
	return nil
}
