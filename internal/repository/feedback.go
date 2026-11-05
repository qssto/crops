package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P2 L8 用户信号的 SQL：驳回关联知识条目（kb_ids）、采纳 / 差评（advice_feedback）。
// §6.9 ② 只给 DDL 与归因 upsert；本文件的查询自拟，均带 tenant_id / farm_id 作用域。

// rejectionKbIDsTx 在驳回事务内取该作业单所属建议卡引用的 kb_id（type='kb'，非空），去重、按字节升序，
// 编码为 JSON 数组；无引用返回 "[]"。文档未给，自拟：work_order 主键 → advice_citation uk_advice_seq 前缀。
func rejectionKbIDsTx(ctx context.Context, tx *gorm.DB, tenantID, farmID, orderID uint64) (string, error) {
	var ids []string
	if err := tx.WithContext(ctx).Raw(
		`SELECT c.kb_id FROM work_order w JOIN advice_citation c ON c.tenant_id=w.tenant_id AND c.farm_id=w.farm_id AND c.advice_id=w.advice_id
 WHERE w.tenant_id=? AND w.farm_id=? AND w.id=? AND c.type='kb' AND c.kb_id IS NOT NULL`,
		tenantID, farmID, orderID).Scan(&ids).Error; err != nil {
		return "", fmt.Errorf("repository: rejection kb_ids order %d: %w", orderID, err)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("repository: encode rejection kb_ids order %d: %w", orderID, err)
	}
	return string(b), nil
}

// FeedbackRepo 是采纳 / 差评写入（advice_feedback）。
type FeedbackRepo struct {
	DB *gorm.DB
}

// AdviceRun 按 Scope 取建议卡的 run_id（反馈的 run_id 只信建议卡，不信请求）。不存在或属他场 → model.ErrNotFound。
// 文档未给，自拟：advice_card 主键 + 作用域。
func (r FeedbackRepo) AdviceRun(ctx context.Context, scope model.Scope, adviceID uint64) (uint64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	var ids []uint64
	if err := r.DB.WithContext(ctx).Raw(`SELECT run_id FROM advice_card WHERE tenant_id=? AND farm_id=? AND id=?`,
		scope.TenantID, scope.FarmID, adviceID).Scan(&ids).Error; err != nil {
		return 0, fmt.Errorf("repository: advice %d run: %w", adviceID, err)
	}
	if len(ids) == 0 {
		return 0, fmt.Errorf("repository: advice %d: %w", adviceID, model.ErrNotFound)
	}
	return ids[0], nil
}

// InsertFeedback 写一条反馈（tenant / farm 取 Scope，忽略 fb 里的值）。request_id 已存在（uk_request）时不改写，
// 首次的 farm / user / advice 与本次一致才返回首次结果与 created=false；任一不一致（他场，或同场他人 / 另一建议卡）
// → 同一个 model.ErrFeedbackRequestConflict，不回首次的行，错误不区分是哪种不一致。
// 文档未给，自拟：INSERT … ON DUPLICATE KEY UPDATE id = id（重放幂等），重放按 (tenant_id, farm_id, request_id) 读回。
func (r FeedbackRepo) InsertFeedback(ctx context.Context, scope model.Scope, fb model.AdviceFeedback) (model.AdviceFeedback, bool, error) {
	if err := requireScope(scope); err != nil {
		return model.AdviceFeedback{}, false, err
	}
	var category any
	if fb.Category != nil {
		category = string(*fb.Category)
	}
	res := r.DB.WithContext(ctx).Exec(
		`INSERT INTO advice_feedback (tenant_id, farm_id, advice_id, run_id, user_id, verdict, category, comment, request_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id`,
		scope.TenantID, scope.FarmID, fb.AdviceID, fb.RunID, fb.UserID, string(fb.Verdict), category, fb.Comment, fb.RequestID, fb.CreatedAt)
	if res.Error != nil {
		return model.AdviceFeedback{}, false, fmt.Errorf("repository: insert advice_feedback: %w", res.Error)
	}
	got, err := r.feedbackByRequest(ctx, scope, fb.RequestID)
	if err != nil {
		return model.AdviceFeedback{}, false, err
	}
	if got.UserID != fb.UserID || got.AdviceID != fb.AdviceID {
		return model.AdviceFeedback{}, false, errFeedbackConflict(fb.RequestID)
	}
	return got, res.RowsAffected == 1, nil
}

// errFeedbackConflict 是 request_id 归属不一致的统一错误（跨农场与同农场同一文本）。
func errFeedbackConflict(requestID string) error {
	return fmt.Errorf("repository: advice_feedback request %q: %w", requestID, model.ErrFeedbackRequestConflict)
}

// feedbackByRequest 按 (tenant_id, farm_id, request_id) 读回一条反馈；本 Scope 无此行（request_id 属他场）→ 冲突。
func (r FeedbackRepo) feedbackByRequest(ctx context.Context, scope model.Scope, requestID string) (model.AdviceFeedback, error) {
	var rows []struct {
		ID, AdviceID, RunID, UserID uint64
		Verdict                     string
		Category, Comment           *string
		CreatedAt                   time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT id, advice_id, run_id, user_id, verdict, category, comment, created_at FROM advice_feedback
 WHERE tenant_id=? AND farm_id=? AND request_id=?`, scope.TenantID, scope.FarmID, requestID).Scan(&rows).Error; err != nil {
		return model.AdviceFeedback{}, fmt.Errorf("repository: read advice_feedback: %w", err)
	}
	if len(rows) == 0 {
		return model.AdviceFeedback{}, errFeedbackConflict(requestID)
	}
	row := rows[0]
	out := model.AdviceFeedback{ID: row.ID, TenantID: scope.TenantID, FarmID: scope.FarmID, AdviceID: row.AdviceID,
		RunID: row.RunID, UserID: row.UserID, Verdict: model.FeedbackVerdict(row.Verdict), Comment: row.Comment,
		RequestID: requestID, CreatedAt: row.CreatedAt}
	if row.Category != nil {
		c := model.RejectReasonCode(*row.Category)
		out.Category = &c
	}
	return out, nil
}
