package app

import (
	"errors"
	"path/filepath"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
)

// ErrMissingFaults：P1 L6 起故障降级矩阵（policy/fault_matrix.yaml + configs/texts/zh-CN.yaml）必须在启动期加载校验。
var ErrMissingFaults = errors.New("app: deps.Faults is required (policy.LoadFaultMatrix)")

// 故障矩阵与文案表的仓库默认路径（cmd/server、cmd/worker、cmd/eval 共用）。
const (
	DefaultFaultMatrixPath = "policy/fault_matrix.yaml"
	DefaultTextsPath       = "configs/texts/zh-CN.yaml"
)

// LoadFaults 按仓库根目录 root 下的默认路径加载故障矩阵与文案表（集成测试 / 评测环境用）。
func LoadFaults(root string) (*policy.FaultMatrix, error) {
	return policy.LoadFaultMatrix(filepath.Join(root, DefaultFaultMatrixPath), filepath.Join(root, DefaultTextsPath))
}

// offlineTextKey 是设备离线通知的文案 key（下发器写入 notify.send payload 与 DEGRADED 事件）。
func offlineTextKey(m *policy.FaultMatrix) string {
	return m.MustFault(model.FaultDeviceOffline).TextKey
}
