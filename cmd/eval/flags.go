package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// P2 第 3 批接缝：run --flags 在每个 trial 的种子农场 FARM 行打开 L4 / L5 / L6 开关，并把设置记入快照 flags；
// resume 按快照 flags 原样重设。不带 --flags 时什么都不加，快照、报告与第 3 批之前逐字节相同。

// flagsUsage 是 run --flags 的说明。
const flagsUsage = "P2 开关，逗号分隔 key=off|shadow|on（bool 形态只认 on / off），写入每个 trial 种子农场的 FARM 行并记入快照 flags；" +
	"可设：rule_offload.operation_window / rule_offload.tank_mix / rule_offload.pest_priority / enable_loader_v1 / enable_compaction"

// evalFlagComment 是 --flags 写入 feature_flag.comment 的取值（列 NOT NULL）。
const evalFlagComment = "eval --flags"

var errEvalFlags = errors.New("eval: bad --flags")

// parseEvalFlags 解析 --flags；空串返回 nil。key 须在 evalkit.FactorFlagKeys 中、不得重复，档位按注册表形态校验。
func parseEvalFlags(arg string) (map[string]string, error) {
	if strings.TrimSpace(arg) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, item := range strings.Split(arg, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			return nil, fmt.Errorf("%w: %q is not key=value", errEvalFlags, item)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: duplicate %s", errEvalFlags, k)
		}
		out[k] = v
	}
	if err := checkEvalFlags(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkEvalFlags 校验 flags（run 解析后与 resume 读快照时共用）。
func checkEvalFlags(flags map[string]string) error {
	reg := model.DefaultFlagRegistry()
	allowed := evalkit.FactorFlagKeys()
	for _, k := range slices.Sorted(maps.Keys(flags)) {
		v := model.FlagMode(flags[k])
		spec := reg[model.FlagKey(k)]
		switch {
		case !slices.Contains(allowed, k):
			return fmt.Errorf("%w: %s is not settable (allowed %s)", errEvalFlags, k, strings.Join(allowed, ","))
		case !v.Valid(), spec.Kind == model.FlagKindBool && v == model.FlagModeShadow:
			return fmt.Errorf("%w: %s=%s (kind %s)", errEvalFlags, k, v, spec.Kind)
		}
	}
	return nil
}

// flagWrites 把 flags 转成种子农场 (tenantID, farmID) 的 FARM 行（按 key 排序，值为规范 JSON）。
func flagWrites(flags map[string]string, tenantID, farmID uint64) ([]model.FlagWrite, error) {
	reg := model.DefaultFlagRegistry()
	out := make([]model.FlagWrite, 0, len(flags))
	for _, k := range slices.Sorted(maps.Keys(flags)) {
		key, mode := model.FlagKey(k), model.FlagMode(flags[k])
		var v model.FlagValue
		if reg[key].Kind == model.FlagKindMode {
			v.Mode = &mode
		} else {
			on := mode == model.FlagModeOn
			v.On = &on
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("eval: marshal flag %s: %w", k, err)
		}
		out = append(out, model.FlagWrite{TenantID: tenantID, FarmID: farmID, Key: key, ScopeType: model.FlagScopeFarm,
			Value: raw, Comment: evalFlagComment})
	}
	return out, nil
}

// setFlags 在任何 trial 开始前把 flags 加进种子（Pool 每次 Acquire 重灌种子）并记下供快照使用；只能调用一次。
func (in *infra) setFlags(flags map[string]string) error {
	if in.flags != nil {
		return errors.New("eval: flags already set")
	}
	if len(flags) == 0 {
		return nil
	}
	if err := checkEvalFlags(flags); err != nil {
		return err
	}
	ws, err := flagWrites(flags, in.seed.Farm.TenantID, in.seed.Farm.FarmID)
	if err != nil {
		return err
	}
	// 种子已有同一 FARM 行时 InsertMissingTx 不覆盖，--flags 会静默失效：直接拒绝。
	for _, w := range ws {
		if slices.ContainsFunc(in.seed.Flags, func(s model.FlagWrite) bool {
			return s.Key == w.Key && s.TenantID == w.TenantID && s.FarmID == w.FarmID
		}) {
			return fmt.Errorf("%w: seed already sets %s on farm (%d,%d)", errEvalFlags, w.Key, w.TenantID, w.FarmID)
		}
	}
	in.seed.Flags = append(in.seed.Flags, ws...)
	in.flags = maps.Clone(flags)
	return nil
}

// withFlags 把已设置的 flags 写进快照：flags 字段原样记录；enable_loader_v1=on 时 loader_strategy 记 v1。
func withFlags(snap evalkit.Snapshot, flags map[string]string) evalkit.Snapshot {
	if len(flags) == 0 {
		return snap
	}
	snap.Flags = maps.Clone(flags)
	if flags[string(model.FlagEnableLoaderV1)] == string(model.FlagModeOn) {
		snap.LoaderStrategy = evalkit.LoaderV1
	}
	return snap
}
