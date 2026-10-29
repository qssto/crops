package repository

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// 落库摘要来源（StoredSummary.Source）。
const (
	SourceToolCall = "tool_call.result_summary"
	SourceRunEvent = "run_event.payload"
)

// StoredSummary 是一个 run 写入业务库的一条脱敏后文本（评测 pii_clean 扫描用）。
type StoredSummary struct {
	Source string // SourceToolCall / SourceRunEvent
	ID     uint64 // tool_call.id 或 run_event.seq
	Text   string
}

// SummaryRepo 读一个 run 已落库的摘要（只读；L5 评测在归还 trial 槽位前调用）。
type SummaryRepo struct{}

// ByRun 按写入顺序返回该 run 的 tool_call.result_summary（非 NULL）与 run_event.payload；db 可为连接。
func (SummaryRepo) ByRun(ctx context.Context, db *gorm.DB, scope model.Scope, runID uint64) ([]StoredSummary, error) {
	type row struct {
		ID   uint64
		Text string
	}
	var calls, events []row
	if err := db.WithContext(ctx).Raw(
		`SELECT id, result_summary AS text FROM tool_call WHERE tenant_id=? AND farm_id=? AND run_id=? AND result_summary IS NOT NULL ORDER BY id`,
		scope.TenantID, scope.FarmID, runID).Scan(&calls).Error; err != nil {
		return nil, fmt.Errorf("repository: list tool_call summaries run %d: %w", runID, err)
	}
	if err := db.WithContext(ctx).Raw(
		`SELECT seq AS id, payload AS text FROM run_event WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY seq`,
		scope.TenantID, scope.FarmID, runID).Scan(&events).Error; err != nil {
		return nil, fmt.Errorf("repository: list run_event payloads run %d: %w", runID, err)
	}
	out := make([]StoredSummary, 0, len(calls)+len(events))
	for _, r := range calls {
		out = append(out, StoredSummary{Source: SourceToolCall, ID: r.ID, Text: r.Text})
	}
	for _, r := range events {
		out = append(out, StoredSummary{Source: SourceRunEvent, ID: r.ID, Text: r.Text})
	}
	return out, nil
}
