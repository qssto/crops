package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// P2 L6 长对话压缩的 agent_run 读写（P2 §6.7 技术实现 2、3、6，定版 SQL 照抄）：会话续接建 run、
// 上下文组装的会话原料、中断 / 终态时随 row_ver 守卫写 compaction_state、压缩触发率。

// sessionChainMax 是沿 prev_run_id 回溯的最大 run 数（含本 run），防止链过长拖慢组装。
const sessionChainMax = 32

// CreateRunLinked 按 link 建 run（agent_run RUNNING），返回 run_id 与是否续接成功。
//   - RunLinkSession：照抄续接 SQL 校验上一个 run（同租户 / 农场 / 地块 / 用户、终态、updated_at ≥ now - Idle），
//     通过则在 INSERT 新 run 的同一事务写 prev_run_id 并复制 compaction_state；
//   - RunLinkSupersede：复检取代的新 run 接替旧 run 在会话链中的位置（prev_run_id 取旧 run 的 prev_run_id），
//     同样复制 compaction_state（文档未给 SQL，校验同租户 / 农场 / 地块）。
//
// 校验不通过（或旧 run 无可续接内容）时开新会话，与 CreateRun 写入相同。
func (r *RunRepo) CreateRunLinked(ctx context.Context, s model.RunState, userID, fieldID uint64, meta agent.RunMeta,
	link model.RunLink) (uint64, bool, error) {
	if err := requireFarm(s.TenantID, s.FarmID); err != nil {
		return 0, false, err
	}
	now := r.clock.Now()
	var (
		id     uint64
		linked bool
	)
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		prev, state, ok, err := r.linkTarget(ctx, tx, s, userID, fieldID, link, now)
		if err != nil {
			return err
		}
		q := `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version, model_tier, knowledge_ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		args := []any{s.TenantID, s.FarmID, fieldID, userID, string(model.RunRunning),
			meta.GraphVersion, meta.PromptVersion, meta.ModelTier, meta.KnowledgeRef, now, now}
		if ok {
			q = `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version, model_tier, knowledge_ref, compaction_state, prev_run_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
			args = []any{s.TenantID, s.FarmID, fieldID, userID, string(model.RunRunning),
				meta.GraphVersion, meta.PromptVersion, meta.ModelTier, meta.KnowledgeRef, state, prev, now, now}
		}
		res, err := execInTx(ctx, tx, q, args...)
		if err != nil {
			return fmt.Errorf("repository: insert agent_run: %w", err)
		}
		last, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: agent_run id: %w", err)
		}
		id, linked = uint64(last), ok
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return id, linked, nil
}

// linkTarget 校验关联的 run，返回新 run 的 prev_run_id（可为 nil）、要复制的 compaction_state（可为 nil）与是否续接。
func (r *RunRepo) linkTarget(ctx context.Context, tx *gorm.DB, s model.RunState, userID, fieldID uint64,
	link model.RunLink, now time.Time) (prev any, state any, ok bool, err error) {
	if link.RunID == 0 {
		return nil, nil, false, nil
	}
	var row struct {
		CompactionState []byte
		PrevRunID       *uint64
	}
	var res *gorm.DB
	switch link.Kind {
	case model.RunLinkSession:
		if link.Idle <= 0 {
			return nil, nil, false, fmt.Errorf("repository: session link run %d: idle %s must be > 0", link.RunID, link.Idle)
		}
		res = tx.WithContext(ctx).Raw(
			`SELECT compaction_state FROM agent_run
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND field_id = ? AND user_id = ?
  AND status IN ('COMPLETED', 'FAILED', 'CANCELED') AND updated_at >= ?`,
			s.TenantID, s.FarmID, link.RunID, fieldID, userID, now.Add(-link.Idle)).Scan(&row)
		row.PrevRunID = &link.RunID
	case model.RunLinkSupersede:
		res = tx.WithContext(ctx).Raw(
			`SELECT compaction_state, prev_run_id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ? AND field_id = ?`,
			s.TenantID, s.FarmID, link.RunID, fieldID).Scan(&row)
	default:
		return nil, nil, false, fmt.Errorf("repository: run link kind %q", link.Kind)
	}
	if res.Error != nil {
		return nil, nil, false, fmt.Errorf("repository: %s link agent_run %d: %w", link.Kind, link.RunID, res.Error)
	}
	if res.RowsAffected == 0 || (row.PrevRunID == nil && row.CompactionState == nil) {
		return nil, nil, false, nil
	}
	if row.CompactionState != nil {
		state = string(row.CompactionState) // json 列不接受 binary 字符集参数
	}
	if row.PrevRunID != nil {
		prev = *row.PrevRunID
	}
	return prev, state, true, nil
}

// compactedUpdate 照抄 §6.7 中断 / 终态 UPDATE：ctx 载体带压缩状态时在调用方事务内执行并返回 true；
// 载体为空返回 false，调用方走 P1 的 UPDATE（开关关闭时 SQL 与 P1 相同）。row_ver 不符 → model.ErrConcurrentUpdate。
func compactedUpdate(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64, status model.RunStatus,
	rowVer int, now time.Time) (bool, error) {
	st, count, ok := agent.CompactionCarrierFrom(ctx).Get()
	if !ok {
		return false, nil
	}
	body, err := json.Marshal(st)
	if err != nil {
		return false, fmt.Errorf("repository: encode compaction_state run %d: %w", runID, err)
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE agent_run
SET compaction_state = ?, compaction_count = ?, status = ?, row_ver = row_ver + 1, updated_at = ?
WHERE tenant_id = ? AND farm_id = ? AND id = ? AND row_ver = ?`,
		string(body), count, string(status), now, scope.TenantID, scope.FarmID, runID, rowVer)
	if res.Error != nil {
		return false, fmt.Errorf("repository: write compaction_state agent_run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return false, fmt.Errorf("repository: agent_run %d row_ver %d: %w", runID, rowVer, model.ErrConcurrentUpdate)
	}
	return true, nil
}

// setRunColumn 在 compactedUpdate 之后补写定版 SQL 未含的列（interrupt_type / reason_code），同一事务、不升 row_ver。
func setRunColumn(ctx context.Context, tx *gorm.DB, scope model.Scope, runID uint64, column string, value any) error {
	var q string
	switch column {
	case "interrupt_type":
		q = `UPDATE agent_run SET interrupt_type = ? WHERE tenant_id = ? AND farm_id = ? AND id = ?`
	case "reason_code":
		q = `UPDATE agent_run SET reason_code = ? WHERE tenant_id = ? AND farm_id = ? AND id = ?`
	default:
		return fmt.Errorf("repository: set agent_run column %q not allowed", column)
	}
	if err := tx.WithContext(ctx).Exec(q, value, scope.TenantID, scope.FarmID, runID).Error; err != nil {
		return fmt.Errorf("repository: set %s agent_run %d: %w", column, runID, err)
	}
	return nil
}

// CompactionRate 照抄压缩触发率 SQL（日报）：[from, to) 内终态 run 中 compaction_count > 0 的比例；无 run 返回 0。
func (r *RunRepo) CompactionRate(ctx context.Context, scope model.Scope, from, to time.Time) (float64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	var rate *float64
	err := r.db.WithContext(ctx).Raw(
		`SELECT AVG(compaction_count > 0) FROM agent_run
WHERE tenant_id = ? AND farm_id = ? AND status IN ('COMPLETED', 'FAILED', 'CANCELED') AND updated_at >= ? AND updated_at < ?`,
		scope.TenantID, scope.FarmID, from, to).Scan(&rate).Error
	if err != nil {
		return 0, fmt.Errorf("repository: compaction rate: %w", err)
	}
	if rate == nil {
		return 0, nil
	}
	return *rate, nil
}

// CompactedRawIDs 列出本 run 转存的被压缩原文（raw_object.kind=COMPACTED_MSGS，按 id 序），供按 run_id 回放（文档未给 SQL）。
func (r *RunRepo) CompactedRawIDs(ctx context.Context, scope model.Scope, runID uint64) ([]uint64, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var ids []uint64
	err := r.db.WithContext(ctx).Raw(
		`SELECT id FROM raw_object WHERE tenant_id = ? AND farm_id = ? AND run_id = ? AND kind = ? ORDER BY id`,
		scope.TenantID, scope.FarmID, runID, string(model.RawCompactedMsgs)).Scan(&ids).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list compacted raw run %d: %w", runID, err)
	}
	return ids, nil
}
