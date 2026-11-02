package agent

import (
	"context"
	"errors"

	"agri-steward/internal/model"
)

// ErrScopeDenied 表示 field_id / 设备 ID 不属于 Scope 的农场（P1 §6.2 功能任务 4，与 MCP 错误码 SCOPE_DENIED 归一）。
var ErrScopeDenied = errors.New("SCOPE_DENIED")

// FieldScopeChecker 校验模型输出或工具参数里的资源归属，越界即写 audit_log(CROSS_FARM_DENIED) 并计数。
// 实现：repository.FieldScopeGuard（app / cmd 装配注入）。
type FieldScopeChecker interface {
	// CheckField 校验 field_id 属于 scope；越界返回 ErrScopeDenied（审计已写）。
	CheckField(ctx context.Context, scope model.Scope, runID, fieldID uint64) error
	// Deny 记录其他越界（如设备 ID）：写审计并计数，target 形如 device:{id}。
	Deny(ctx context.Context, scope model.Scope, runID uint64, target string) error
}

// DeviceOwnership 判定设备归属：按设备显式绑定的 (tenant_id, farm_id) 判，不经地块编码推断
// （ext_field_code 只在农场内唯一，见 uk_ext）。设备不存在与属于他场一律返回 false，不暴露存在性。
// gateway 对入参 device_id 统一调用，越界经 FieldScopeChecker.Deny 写 CROSS_FARM_DENIED。
// 实现：device.Bindings（mock 模式读 configs/mock/device.yaml；real 模式暂为空绑定，全部拒绝，真实绑定待 P1-L2）。
type DeviceOwnership interface {
	OwnsDevice(ctx context.Context, scope model.Scope, deviceID string) (bool, error)
}

// FarmRegionSource 取农场 farm_profile.region 原值（如 江苏），graph 经 knowledge.RegionCode 归一后按区域过滤知识。
// 实现：repository.FarmRegions（app 装配注入）；未注入时用配置的默认区域。
type FarmRegionSource interface {
	FarmRegion(ctx context.Context, scope model.Scope) (string, error)
}
