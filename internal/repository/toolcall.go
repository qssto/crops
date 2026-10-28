package repository

import (
	"context"
	"encoding/json"
	"fmt"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ToolCallRepo 是 tool_call 读写（agent.ToolCallStore 的实现），SQL 照抄 P0 §6.3。
type ToolCallRepo struct {
	db *gorm.DB
}

// NewToolCallRepo 构造 ToolCallRepo。
func NewToolCallRepo(db *gorm.DB) *ToolCallRepo { return &ToolCallRepo{db: db} }

// Save 在一个事务内：取 attempt = 已有最大 + 1 → 落行（同 attempt 重复写入返回已有行 id）→
// OK 且时效时作废同 run、同工具、更小 epoch 的结果。LAST_INSERT_ID 与 INSERT 同事务同连接。
func (r *ToolCallRepo) Save(ctx context.Context, row model.ToolCallRow) (uint64, error) {
	var id uint64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var attempt int
		if err := tx.Raw(`SELECT COALESCE(MAX(attempt), 0) + 1 FROM tool_call WHERE tenant_id=? AND farm_id=? AND call_key=?`,
			row.TenantID, row.FarmID, row.CallKey).Scan(&attempt).Error; err != nil {
			return fmt.Errorf("repository: tool_call attempt %s: %w", row.CallKey, err)
		}
		res, err := execInTx(ctx, tx,
			`INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, epoch, tool_name, policy_level,
  args_digest, status, error_code, source, data_freshness_sec, time_sensitive, result_summary, raw_object_id, latency_ms, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`,
			row.TenantID, row.FarmID, row.RunID, row.StepID, row.CallKey, attempt, row.Epoch, row.ToolName, row.PolicyLevel,
			row.ArgsDigest, row.Status, nullIfEmpty(row.ErrorCode), nullIfEmpty(row.Source), row.DataFreshnessSec,
			row.TimeSensitive, nullJSON(row.ResultSummary), row.RawObjectID, row.LatencyMS, row.CreatedAt.UTC())
		if err != nil {
			return fmt.Errorf("repository: insert tool_call %s: %w", row.CallKey, err)
		}
		last, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("repository: tool_call id: %w", err)
		}
		id = uint64(last)
		if row.Status != model.ToolStatusOK || !row.TimeSensitive {
			return nil
		}
		err = tx.Exec(`UPDATE tool_call SET superseded_at=? WHERE tenant_id=? AND farm_id=? AND run_id=? AND tool_name=?
   AND time_sensitive=1 AND epoch<? AND superseded_at IS NULL`,
			row.CreatedAt.UTC(), row.TenantID, row.FarmID, row.RunID, row.ToolName, row.Epoch).Error
		if err != nil {
			return fmt.Errorf("repository: supersede tool_call run %d %s: %w", row.RunID, row.ToolName, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// FindOK 是去重回查：只认成功结果，取 attempt 最大的一行。
func (r *ToolCallRepo) FindOK(ctx context.Context, scope model.Scope, callKey string) (model.ToolCallHit, bool, error) {
	var row struct {
		ID            uint64
		ResultSummary []byte
		RawObjectID   *uint64
	}
	res := r.db.WithContext(ctx).Raw(`SELECT id, result_summary, raw_object_id FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND call_key=? AND status='OK' ORDER BY attempt DESC LIMIT 1`,
		scope.TenantID, scope.FarmID, callKey).Scan(&row)
	if res.Error != nil {
		return model.ToolCallHit{}, false, fmt.Errorf("repository: find ok tool_call %s: %w", callKey, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.ToolCallHit{}, false, nil
	}
	return model.ToolCallHit{ID: row.ID, ResultSummary: row.ResultSummary, RawObjectID: row.RawObjectID}, true, nil
}

// nullJSON 把空 json 写成 NULL。
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// FieldScope 校验地块归属（mcp.FieldScope 的实现）：gateway 调用 Server 前的租户隔离检查。
type FieldScope struct {
	DB *gorm.DB
}

// Owns 判断 field_id 属于 scope 的 tenant_id / farm_id。
func (s FieldScope) Owns(ctx context.Context, scope model.Scope, fieldID uint64) (bool, error) {
	var one int
	res := s.DB.WithContext(ctx).Raw(`SELECT 1 FROM field_profile WHERE id=? AND tenant_id=? AND farm_id=?`,
		fieldID, scope.TenantID, scope.FarmID).Scan(&one)
	if res.Error != nil {
		return false, fmt.Errorf("repository: field scope %d: %w", fieldID, res.Error)
	}
	return res.RowsAffected > 0, nil
}
