package repository

import (
	"errors"
	"fmt"

	"agri-steward/internal/model"
)

// ErrScopeRequired 表示业务方法收到零值 Scope（tenant_id 或 farm_id 为 0）。
var ErrScopeRequired = errors.New("repository: scope tenant_id/farm_id required")

// requireScope 是业务方法的第一道闸：零值 Scope 直接拒绝。
func requireScope(scope model.Scope) error { return requireFarm(scope.TenantID, scope.FarmID) }

// requireFarm 用于参数自带归属（OrderRef / TicketRow / RunKey 等）的方法。
func requireFarm(tenantID, farmID uint64) error {
	if tenantID == 0 || farmID == 0 {
		return fmt.Errorf("%w: got (%d,%d)", ErrScopeRequired, tenantID, farmID)
	}
	return nil
}

// SystemScope 是跨农场扫描的显式凭证：只给 worker 类任务用，SQL 只走注释为「worker 专用」的索引。
// 构造点受 archtest 白名单约束（P1-L4 起收紧到 cmd/worker）。
type SystemScope struct{ task string }

// NewSystemScope 以任务名构造凭证（任务名进日志，便于追溯哪个扫描）。
func NewSystemScope(task string) SystemScope { return SystemScope{task: task} }

// Task 返回扫描任务名。
func (s SystemScope) Task() string { return s.task }

// requireSystem 拒绝零值 SystemScope（未经 NewSystemScope 构造）。
func requireSystem(s SystemScope) error {
	if s.task == "" {
		return fmt.Errorf("%w: zero SystemScope", ErrScopeRequired)
	}
	return nil
}
