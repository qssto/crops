package constraint

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"agri-steward/internal/model"
)

// ErrBadRegistry 表示注册表构造失败（rule_id 重复、为空或超列宽）。
var ErrBadRegistry = errors.New("bad constraint registry")

// Registry 是约束校验器注册表：按 rule_id 索引，按作业类型 / 设备 / 阶段给出必检集合（Plan）。
// 不在注册表里的规则不产生任何影响；必检集合里缺失的规则、空集合都按失败行输出（失败关闭）。
type Registry struct {
	byID  map[string]Checker
	order []string // 注册顺序（输出顺序：前置条件在前）
}

// NewRegistry 按给定顺序注册；rule_id 为空、超过 RuleIDMax 或重复返回 ErrBadRegistry。
func NewRegistry(cs ...Checker) (*Registry, error) {
	r := &Registry{byID: make(map[string]Checker, len(cs))}
	for _, c := range cs {
		id := c.RuleID()
		switch {
		case id == "" || len(id) > RuleIDMax:
			return nil, fmt.Errorf("%w: rule_id %q length", ErrBadRegistry, id)
		case r.byID[id] != nil:
			return nil, fmt.Errorf("%w: duplicate rule_id %q", ErrBadRegistry, id)
		}
		r.byID[id] = c
		r.order = append(r.order, id)
	}
	return r, nil
}

// Default 是内置注册表：法规前置条件 + 天气 / 间隔期 / 重复作业 / 空域 / 漂移 / 设备。
func Default() *Registry {
	r, err := NewRegistry(registeredPrecondition{}, windChecker(), rainChecker(), tempChecker(), phiChecker{},
		repeatChecker{}, noFlyChecker{}, driftChecker{}, batteryChecker{}, payloadChecker{})
	if err != nil {
		panic(err) // 内置集合是常量，构造失败是编程错误（registry_test 覆盖）
	}
	return r
}

// Has 表示 rule_id 已注册。
func (r *Registry) Has(id string) bool { return r.byID[id] != nil }

// IsPrecondition 表示 rule_id 是法规前置条件（无工具来源，不要求 tool_call_id）。
func (r *Registry) IsPrecondition(id string) bool {
	p, ok := r.byID[id].(Precondition)
	return ok && p.Precondition()
}

// Required 是该阶段 / 作业类型 / 设备的必检 rule_id（按注册顺序）：
//   - 施药：登记前置条件、风速、降雨、气温、安全间隔期、重复作业、漂移；无人机加禁飞区；
//   - 追肥：喷雾类设备（叶面喷施）为风速、降雨、气温、重复作业，无人机加禁飞区；人工撒施为降雨、重复作业；
//   - 灌溉：重复作业；
//   - 下发前复检（PRE_DISPATCH）无人机另加电量与载药量。
func (r *Registry) Required(phase model.ConstraintPhase, wt model.WorkType, device string) []string {
	var want []string
	switch wt {
	case model.WorkSpray:
		want = []string{RuleRegistered, RuleWind, RuleRain, RuleTemp, RulePHI, RuleRepeat, RuleDrift}
	case model.WorkFertilize:
		want = []string{RuleRain, RuleRepeat}
		if sprayDevice(device) {
			want = append(want, RuleWind, RuleTemp)
		}
	case model.WorkIrrigate:
		want = []string{RuleRepeat}
	default:
		return nil
	}
	if device == DeviceUAV {
		want = append(want, RuleNoFly)
		if phase == model.PhasePreDispatch {
			want = append(want, RuleBattery, RulePayload)
		}
	}
	return r.sorted(want)
}

// sorted 按注册顺序排列；未注册的 rule_id 排在最后（Evaluate 输出缺失失败行）。
func (r *Registry) sorted(ids []string) []string {
	pos := func(id string) int {
		if i := slices.Index(r.order, id); i >= 0 {
			return i
		}
		return len(r.order)
	}
	slices.SortStableFunc(ids, func(a, b string) int { return pos(a) - pos(b) })
	return slices.Compact(ids)
}

// Evaluate 只跑必检集合：未注册的必检项与空集合输出失败行。Attempt 留 0，由落库方取 ticket_seq / recheck_seq。
func (r *Registry) Evaluate(in *Input) []model.ConstraintCheck {
	ids := r.Required(in.Phase, in.WorkType, in.DeviceType)
	if len(ids) == 0 {
		return []model.ConstraintCheck{Missing(in.Phase, RuleNoPlan, "no constraint plan for "+string(in.WorkType)+"/"+in.DeviceType)}
	}
	out := make([]model.ConstraintCheck, 0, len(ids))
	for _, id := range ids {
		c := r.byID[id]
		if c == nil {
			out = append(out, Missing(in.Phase, id, "checker not registered"))
			continue
		}
		out = append(out, Row(in.Phase, id, c.Check(in)))
	}
	return out
}

// Row 把一条结论编码为 constraint_check 行（Observed / Threshold 均为纯值结构，编码不会失败）。
func Row(phase model.ConstraintPhase, id string, res Result) model.ConstraintCheck {
	return model.ConstraintCheck{Phase: phase, RuleID: id, Passed: res.Passed, Observed: mustJSON(res.Observed),
		Threshold: mustJSON(res.Threshold)}
}

// Missing 是必检项缺失 / 未映射法规条目的失败行。
func Missing(phase model.ConstraintPhase, id, note string) model.ConstraintCheck {
	return Row(phase, id, Result{Observed: Observed{Note: note}, Threshold: Threshold{Signoff: SignoffPending}})
}

// encodeFailed 是编码失败时的固定占位（字面量，不再二次编码）：行仍是失败可读的 JSON 对象，而非空列。
var encodeFailed = json.RawMessage(`{"note":"encode failed"}`)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Observed.Value 只取 float64 / int / bool / nil；出错即编程错误，落固定占位
		return encodeFailed
	}
	return b
}
