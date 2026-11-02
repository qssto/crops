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

// LocalOrderRepo 实现 agent.LocalOrderSource（P1 L3）：同地块在途或近期完成的 REAL 单，走 idx_field_status。
type LocalOrderRepo struct {
	DB *gorm.DB
}

var _ agent.LocalOrderSource = LocalOrderRepo{}

// RecentOrders 实现 agent.LocalOrderSource（P1 §6.4 功能任务 3 定版 SQL）：DRY_RUN 单不计；since 为 now − 最长回看窗口。
func (r LocalOrderRepo) RecentOrders(ctx context.Context, scope model.Scope, fieldID, excludeOrderID uint64,
	since time.Time) ([]agent.LocalOrder, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []struct {
		ID        uint64
		WorkType  string
		Payload   []byte
		Status    string
		UpdatedAt time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT id, work_type, payload, status, updated_at FROM work_order WHERE tenant_id=? AND farm_id=? AND field_id=? AND dispatch_mode='REAL' AND id<>? AND (status IN ('APPROVED','DISPATCHED','EXECUTING') OR (status IN ('DONE','PARTIALLY_DONE') AND updated_at >= ?))`,
		scope.TenantID, scope.FarmID, fieldID, excludeOrderID, since).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: local orders field %d: %w", fieldID, err)
	}
	out := make([]agent.LocalOrder, 0, len(rows))
	for _, row := range rows {
		out = append(out, agent.LocalOrder{OrderID: row.ID, WorkType: model.WorkType(row.WorkType),
			Payload: json.RawMessage(row.Payload), Status: model.WorkOrderStatus(row.Status), UpdatedAt: row.UpdatedAt})
	}
	return out, nil
}
