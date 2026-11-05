package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P2 L4 规则下沉影子比对（§6.5 技术实现）：shadow 档的比对行与建议卡同一事务写 rule_shadow_diff；
// 影子期按农场查差异率；worker 按农场逐项 DELETE … LIMIT 5000 清理超过保留期的行。

// ShadowPurgeBatch 是清理单批上限（§6.5：DELETE … LIMIT 5000）。
const ShadowPurgeBatch = 5000

// SaveAdviceWithShadow 在 SaveAdvice 的同一事务里写 rule_shadow_diff（graph.ShadowAdviceStore）：
// 出卡（outcome=ADVICE）时 advice_id 取卡片 id，拒答为 0；uk_run_item 冲突 ON DUPLICATE KEY UPDATE id = id（重放幂等）。
// SaveAdvice 在外层事务句柄上再开事务即 GORM 嵌套事务（SAVEPOINT），落卡、引用、记忆与影子行同成同败。
func (r *AdviceRepo) SaveAdviceWithShadow(ctx context.Context, scope model.Scope, card model.AdviceCard,
	diffs []model.RuleShadowDiff) (uint64, error) {
	if len(diffs) == 0 {
		return r.SaveAdvice(ctx, scope, card)
	}
	for i, d := range diffs {
		if err := checkScope(scope, d.TenantID, d.FarmID); err != nil {
			return 0, fmt.Errorf("repository: rule_shadow_diff[%d]: %w", i, err)
		}
		if d.RunID != card.RunID {
			return 0, fmt.Errorf("repository: rule_shadow_diff[%d] run %d does not match advice run %d", i, d.RunID, card.RunID)
		}
	}
	var adviceID uint64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		id, err := (&AdviceRepo{db: tx, clock: r.clock}).SaveAdvice(ctx, scope, card)
		if err != nil {
			return err
		}
		adviceID = id
		ref := uint64(0)
		if card.Outcome == model.OutcomeAdvice {
			ref = id
		}
		for _, d := range diffs {
			if err := insertShadowDiffTx(ctx, tx, scope, ref, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return adviceID, nil
}

// insertShadowDiffTx 写一行 rule_shadow_diff（created_at 为零或 json 列为空时拒写：两列 NOT NULL）。
func insertShadowDiffTx(ctx context.Context, tx *gorm.DB, scope model.Scope, adviceID uint64, d model.RuleShadowDiff) error {
	if d.CreatedAt.IsZero() || len(d.RuleOutput) == 0 || len(d.ModelOutput) == 0 {
		return fmt.Errorf("repository: rule_shadow_diff run %d %s: created_at / rule_output / model_output required", d.RunID, d.Item)
	}
	if _, err := execInTx(ctx, tx,
		`INSERT INTO rule_shadow_diff (tenant_id, farm_id, run_id, advice_id, item, agreed, diff_kind, rule_output, model_output, rule_ver, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE id = id`,
		scope.TenantID, scope.FarmID, d.RunID, adviceID, string(d.Item), d.Agreed, nullIfEmpty(string(d.DiffKind)),
		string(d.RuleOutput), string(d.ModelOutput), d.RuleVer, d.CreatedAt); err != nil {
		return fmt.Errorf("repository: insert rule_shadow_diff run %d %s: %w", d.RunID, d.Item, err)
	}
	return nil
}

// RuleShadowDiffRepo 是 rule_shadow_diff 的查询与清理仓储。
type RuleShadowDiffRepo struct{ DB *gorm.DB }

// DiffRates 照抄 §6.5：影子期本农场各下沉项差异率（agreed=0 占比），since 起（含）。无行的项不出现在结果里。
func (r RuleShadowDiffRepo) DiffRates(ctx context.Context, scope model.Scope, since time.Time) (_ map[model.RuleOffloadItem]float64, err error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	rows, err := r.DB.WithContext(ctx).Raw(
		`SELECT item, AVG(agreed = 0) FROM rule_shadow_diff WHERE tenant_id = ? AND farm_id = ? AND created_at >= ? GROUP BY item`,
		scope.TenantID, scope.FarmID, since).Rows()
	if err != nil {
		return nil, fmt.Errorf("repository: rule_shadow_diff rates farm %d: %w", scope.FarmID, err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("repository: close rule_shadow_diff rates: %w", cerr))
		}
	}()
	out := map[model.RuleOffloadItem]float64{}
	for rows.Next() {
		var item string
		var rate float64
		if err := rows.Scan(&item, &rate); err != nil { // 无别名的聚合列按位置扫描（SQL 照抄 §6.5）
			return nil, fmt.Errorf("repository: scan rule_shadow_diff rate farm %d: %w", scope.FarmID, err)
		}
		out[model.RuleOffloadItem(item)] = rate
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repository: rule_shadow_diff rates farm %d: %w", scope.FarmID, err)
	}
	return out, nil
}

// FarmRef 定位一个农场。
type FarmRef struct {
	TenantID, FarmID uint64
}

// ShadowFarms 列出有早于 before 的影子行的农场（清理任务逐农场执行）：按 idx_scope_item 前缀 (tenant_id, farm_id)
// 顺序分组、HAVING MIN(created_at) 过滤，免去对过期行的 DISTINCT 临时表；结果集以农场数为界。
func (r RuleShadowDiffRepo) ShadowFarms(ctx context.Context, sys SystemScope, before time.Time) ([]FarmRef, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var out []FarmRef
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id FROM rule_shadow_diff GROUP BY tenant_id, farm_id HAVING MIN(created_at) < ? ORDER BY tenant_id, farm_id`,
		before).Scan(&out).Error; err != nil {
		return nil, fmt.Errorf("repository: rule_shadow_diff farms (%s): %w", sys.Task(), err)
	}
	return out, nil
}

// PurgeBatch 照抄 §6.5 删一批（≤ ShadowPurgeBatch 行）本农场该项早于 before 的影子行，返回删除行数；
// 调用方在返回值 = ShadowPurgeBatch 时继续下一批。
func (r RuleShadowDiffRepo) PurgeBatch(ctx context.Context, sys SystemScope, farm FarmRef, item model.RuleOffloadItem,
	before time.Time) (int64, error) {
	if err := requireSystem(sys); err != nil {
		return 0, err
	}
	if err := requireFarm(farm.TenantID, farm.FarmID); err != nil {
		return 0, err
	}
	res := r.DB.WithContext(ctx).Exec(
		`DELETE FROM rule_shadow_diff WHERE tenant_id = ? AND farm_id = ? AND item = ? AND created_at < ? LIMIT 5000`,
		farm.TenantID, farm.FarmID, string(item), before)
	if res.Error != nil {
		return 0, fmt.Errorf("repository: purge rule_shadow_diff farm %d %s (%s): %w", farm.FarmID, item, sys.Task(), res.Error)
	}
	return res.RowsAffected, nil
}
