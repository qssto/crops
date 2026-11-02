package device

import (
	"context"
	"slices"

	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/model"
)

// farmKey 是设备显式绑定的 (tenant_id, farm_id)；farm_id 只在租户内唯一（uk_farm），必须带租户。
type farmKey struct{ tenant, farm uint64 }

// Bindings 是设备 → (tenant_id, farm_id) 的显式绑定，实现 agent.DeviceOwnership（gateway 统一校验 device_id）。
// mock 模式取自 configs/mock/device.yaml；real 模式暂为空绑定（全部拒绝），真实绑定源待 P1-L2（cmd/devicesim + Adapter）。
// 不按设备可服务的地块编码推断归属：ext_field_code 只在农场内唯一（uk_ext）。
type Bindings struct {
	owner map[string]farmKey
}

// NewBindings 由设备清单构造绑定（清单须已通过 Data.validate）。
func NewBindings(devices []Device) *Bindings {
	b := &Bindings{owner: make(map[string]farmKey, len(devices))}
	for _, dv := range devices {
		b.owner[dv.DeviceID] = farmKey{dv.TenantID, dv.FarmID}
	}
	return b
}

// LoadBindings 严格读取 device mock 数据并取出绑定（评测与 gateway 共用同一份清单）。
func LoadBindings(path string) (*Bindings, error) {
	var d Data
	if err := kit.LoadYAML(path, &d); err != nil {
		return nil, err
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return NewBindings(d.Devices), nil
}

// OwnsDevice 实现 agent.DeviceOwnership：设备不存在、属于他场、scope 缺租户或农场一律 false。
func (b *Bindings) OwnsDevice(_ context.Context, scope model.Scope, deviceID string) (bool, error) {
	return b.owns(scope, deviceID), nil
}

// DeviceIDs 返回绑定到 scope 农场的全部设备 ID（升序），供评测按结构判定 device_id 归属。
func (b *Bindings) DeviceIDs(_ context.Context, scope model.Scope) ([]string, error) {
	out := []string{}
	for id := range b.owner {
		if b.owns(scope, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (b *Bindings) owns(scope model.Scope, deviceID string) bool {
	if scope.TenantID == 0 || scope.FarmID == 0 {
		return false
	}
	k, ok := b.owner[deviceID]
	return ok && k == farmKey{scope.TenantID, scope.FarmID}
}
