// Package adapter 是设备 Adapter（mock / real），只被 service/workorder 调用（母文档 §4.6.2-2）。
// L1 只冻结接口；MockAdapter 在 L8 实现，RealAdapter 在 P1 实现。
package adapter

import (
	"context"
	"encoding/json"

	"agri-steward/internal/model"
)

// DryRunResult 是 dry-run 返回的将下发指令（进程内，无外部 IO）。
type DryRunResult struct {
	Commands json.RawMessage
}

// DeviceAdapter 是设备下发接缝。
// 实现：adapter.MockAdapter（P0 L8，DryRun）；adapter.RealAdapter（P1，Dispatch）。
type DeviceAdapter interface {
	DryRun(ctx context.Context, order model.WorkOrder) (DryRunResult, error)
	Dispatch(ctx context.Context, order model.WorkOrder, idempotencyKey string) (deviceJobID string, err error)
}
