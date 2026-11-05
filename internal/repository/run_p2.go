package repository

import (
	"context"
	"fmt"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// agent_run P2 列（迁移 0014_agent_run_p2，P2 第 3 批接缝）。L5 / L6 在本文件之外各开新文件加方法，不改 run.go。

var _ agent.RunUsageStore = (*RunRepo)(nil)

// loaderStrategyMax 与 agent_run.loader_strategy varchar(16) 一致。
const loaderStrategyMax = 16

// SetLoaderStrategy 写本 run 的知识加载策略（L5；不改状态、不升 row_ver）。
func (r *RunRepo) SetLoaderStrategy(ctx context.Context, scope model.Scope, runID uint64, strategy string) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	if strategy == "" || len(strategy) > loaderStrategyMax {
		return fmt.Errorf("repository: agent_run %d loader_strategy %q: want 1-%d bytes", runID, strategy, loaderStrategyMax)
	}
	err := r.db.WithContext(ctx).Exec(
		`UPDATE agent_run SET loader_strategy = ? WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		strategy, scope.TenantID, scope.FarmID, runID).Error
	if err != nil {
		return fmt.Errorf("repository: set loader_strategy agent_run %d: %w", runID, err)
	}
	return nil
}

// AddTokens 照抄 P2 §6.7 token 累计 SQL（L6；不改状态、不升 row_ver）。负数拒绝；两者都为 0 时不写库。
func (r *RunRepo) AddTokens(ctx context.Context, scope model.Scope, runID uint64, prompt, completion int) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	if prompt < 0 || completion < 0 {
		return fmt.Errorf("repository: agent_run %d tokens (%d,%d): negative", runID, prompt, completion)
	}
	if prompt == 0 && completion == 0 {
		return nil
	}
	err := r.db.WithContext(ctx).Exec(
		`UPDATE agent_run SET prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ? WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		prompt, completion, scope.TenantID, scope.FarmID, runID).Error
	if err != nil {
		return fmt.Errorf("repository: add tokens agent_run %d: %w", runID, err)
	}
	return nil
}

// Usage 读本 run 的 P2 列；run 不在 Scope 内返回 model.ErrNotFound。
func (r *RunRepo) Usage(ctx context.Context, scope model.Scope, runID uint64) (model.RunUsage, error) {
	if err := requireScope(scope); err != nil {
		return model.RunUsage{}, err
	}
	var u model.RunUsage
	res := r.db.WithContext(ctx).Raw(
		`SELECT loader_strategy, prompt_tokens, completion_tokens, compaction_count, prev_run_id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		scope.TenantID, scope.FarmID, runID).Scan(&u)
	if res.Error != nil {
		return model.RunUsage{}, fmt.Errorf("repository: get agent_run %d usage: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.RunUsage{}, fmt.Errorf("repository: agent_run %d: %w", runID, model.ErrNotFound)
	}
	return u, nil
}
