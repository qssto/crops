package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// CrossFarmDeniedMetric 是跨农场拒绝计数名（L7 前以 slog 结构化日志计数，HTTP 侧计数归 L1-A）。
const CrossFarmDeniedMetric = "as_cross_farm_denied_total"

// FieldScopeGuard 实现 agent.FieldScopeChecker：查 field_profile 归属，越界写 CROSS_FARM_DENIED 审计并计数。
type FieldScopeGuard struct {
	DB     *gorm.DB
	Clock  clock.Clock
	Logger *slog.Logger
}

var _ agent.FieldScopeChecker = FieldScopeGuard{}

// CheckField 实现 agent.FieldScopeChecker（P1 §6.2 功能任务 4 定版 SQL）。
func (g FieldScopeGuard) CheckField(ctx context.Context, scope model.Scope, runID, fieldID uint64) error {
	owns, err := FieldScope{DB: g.DB}.Owns(ctx, scope, fieldID)
	if err != nil {
		return err
	}
	if owns {
		return nil
	}
	if err := g.Deny(ctx, scope, runID, ref("field", fieldID)); err != nil {
		return err
	}
	return fmt.Errorf("%w: field %d not in farm (%d,%d)", agent.ErrScopeDenied, fieldID, scope.TenantID, scope.FarmID)
}

// FarmRegions 实现 agent.FarmRegionSource：读本农场 farm_profile.region 原值（uk_farm）。
type FarmRegions struct {
	DB *gorm.DB
}

var _ agent.FarmRegionSource = FarmRegions{}

// FarmRegion 返回农场区域原值；农场不存在返回 model.ErrNotFound。
func (r FarmRegions) FarmRegion(ctx context.Context, scope model.Scope) (string, error) {
	if err := requireScope(scope); err != nil {
		return "", err
	}
	var region string
	res := r.DB.WithContext(ctx).Raw(`SELECT region FROM farm_profile WHERE tenant_id=? AND farm_id=?`,
		scope.TenantID, scope.FarmID).Scan(&region)
	if res.Error != nil {
		return "", fmt.Errorf("repository: farm_profile region (%d,%d): %w", scope.TenantID, scope.FarmID, res.Error)
	}
	if res.RowsAffected == 0 {
		return "", fmt.Errorf("repository: farm_profile (%d,%d): %w", scope.TenantID, scope.FarmID, model.ErrNotFound)
	}
	return region, nil
}

// Deny 实现 agent.FieldScopeChecker：独立短写审计（不随业务回滚）并打计数日志。
func (g FieldScopeGuard) Deny(ctx context.Context, scope model.Scope, runID uint64, target string) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"code": "SCOPE_DENIED", "target": target, "source": "agent"})
	if err != nil {
		return fmt.Errorf("repository: encode cross-farm audit: %w", err)
	}
	var rid *uint64
	if runID != 0 {
		rid = &runID
	}
	rec := NewAuditRecord(scope, rid, model.Actor{Type: model.ActorAgent, ID: scope.UserID}, model.AuditCrossFarmDenied,
		target, payload, model.AuditResultDenied, g.Clock.Now())
	if err := (AuditRepo{DB: g.DB}).Append(ctx, rec); err != nil {
		return fmt.Errorf("repository: CROSS_FARM_DENIED %s: %w", target, err)
	}
	if g.Logger != nil {
		g.Logger.WarnContext(ctx, "cross farm denied", "metric", CrossFarmDeniedMetric, "tenant_id", scope.TenantID,
			"farm_id", scope.FarmID, "run_id", runID, "target", target, "source", "agent")
	}
	return nil
}
