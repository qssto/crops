package agent

import (
	"context"

	"agri-steward/internal/model"
)

// ---- P1 L0：开关接缝 ----

// DispatchModeSource 决定新建作业单的 dispatch_mode（P1 §6.1 流程 5）：to_order 时读一次
// enable_robot_dispatch（直读、不走缓存），开 → REAL，关 → DRY_RUN。实现：service.FlagService，app 注入 graph。
// 返回错误时调用方按 DRY_RUN 写定（失败关闭）。
type DispatchModeSource interface {
	DispatchMode(ctx context.Context, scope model.Scope) (model.DispatchMode, error)
}

// DeviceDispatchModeSource 按设备类型决定新建作业单的 dispatch_mode（P1 §6.3 功能任务 3）：
// enable_robot_dispatch 开 **且** 设备类型在 adapter.Registry 可开放集合中才 REAL，否则 DRY_RUN。
// 实现：app 组合 DispatchModeSource（FlagService）与 Registry.Openable；返回错误时调用方按 DRY_RUN 写定。
type DeviceDispatchModeSource interface {
	DispatchModeFor(ctx context.Context, scope model.Scope, deviceType string) (model.DispatchMode, error)
}
