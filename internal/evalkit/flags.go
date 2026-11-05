package evalkit

import (
	"slices"

	"agri-steward/internal/model"
)

// LoaderV1 是 enable_loader_v1 开时快照的 loader_strategy（P2 L5）。
const LoaderV1 = "v1"

// FlagFactors 是快照 flags 中各自成为可比因子的开关（P2 第 3 批接缝，顺序即报告顺序）：L4 rule_offload.* 与
// L6 enable_compaction。enable_loader_v1 不单列，它体现为 loader_strategy（关 v0 / 开 v1），避免同一改动算两个因子。
func FlagFactors() []string {
	keys := make([]string, 0, 4)
	for _, k := range model.RuleOffloadFlags() {
		keys = append(keys, string(k))
	}
	return append(keys, string(model.FlagEnableCompaction))
}

// FactorFlagKeys 是 cmd/eval --flags 可设置、记入快照 flags 的开关：FlagFactors 加 enable_loader_v1。
func FactorFlagKeys() []string {
	return append(FlagFactors(), string(model.FlagEnableLoaderV1))
}

// FlagState 返回快照里开关的档位；没有记录（P2 第 3 批前的快照、默认运行）按 off，与开关默认值一致，
// 故 baseline_p1 这类旧快照与「开关全关」的新快照可比。
func (s Snapshot) FlagState(key string) string {
	if v := s.Flags[key]; v != "" {
		return v
	}
	return string(model.FlagModeOff)
}

// badFlags 报告快照 flags 有不可设置的 key 或非法档位（bool 形态只认 on / off）。
func badFlags(s Snapshot) bool {
	reg := model.DefaultFlagRegistry()
	allowed := FactorFlagKeys()
	for k, v := range s.Flags {
		spec := reg[model.FlagKey(k)]
		m := model.FlagMode(v)
		switch {
		case !slices.Contains(allowed, k), !m.Valid(),
			spec.Kind == model.FlagKindBool && m == model.FlagModeShadow:
			return true
		}
	}
	return false
}
