package constraint

import (
	"fmt"
	"math"
	"slices"
	"time"

	"agri-steward/internal/agent/knowledge"
)

// 约束阈值所在知识条目（阈值只来自知识库；新增条目均为 claude-draft / pending_expert）。
const (
	KBSpray     = "common/operation_window/spray"
	KBFertilize = "common/operation_window/fertilize"
	KBUAVOp     = "common/operation_window/uav"
	KBInterval  = "common/work_constraint/interval"
	KBAirspace  = "common/work_constraint/airspace"
	KBDrift     = "common/work_constraint/drift"
	KBUAVDevice = "common/work_constraint/uav-device"
)

// 知识条目里的阈值 / 参数 id。
const (
	thWindKnapsack   = "wind-knapsack"
	thWindUAV        = "wind-uav"
	thRain24h        = "rain-24h"
	thTempMax        = "temp-max"
	thWeatherStale   = "weather-staleness"
	thPHIMargin      = "phi-margin"
	thRepeatSpray    = "repeat-spray"
	thRepeatFert     = "repeat-fertilize"
	thRepeatIrrigate = "repeat-irrigate"
	thNoFlyBuffer    = "no-fly-buffer"
	thSensitiveBuf   = "sensitive-buffer"
	thDriftUAV       = "drift-buffer-uav"
	thDriftKnapsack  = "drift-buffer-knapsack"
	thDriftSector    = "drift-sector-half-angle"
	thDeviceStale    = "device-staleness"
	thBatteryMargin  = "battery-margin"
	thPayloadMargin  = "payload-margin"
	paramTankL       = "uav_tank_capacity_l"
	paramChargeMu    = "uav_full_charge_mu"
	paramSprayVolume = "uav_spray_volume"
	reviewPending    = "pending_expert"
)

// threshold 取条目 kbID 的阈值 id；条目或阈值缺失返回 false（调用方判失败，threshold 只带 kb_id 与阈值 id）。
func threshold(in *Input, kbID, id string) (Threshold, bool) {
	missing := Threshold{KB: "kb:" + kbID, ThresholdID: id, Signoff: SignoffPending}
	if in.KB == nil {
		return missing, false
	}
	en, ok := in.KB.Entry(kbID)
	if !ok {
		return missing, false
	}
	i := slices.IndexFunc(en.Thresholds, func(t knowledge.Threshold) bool { return t.ID == id })
	if i < 0 {
		return missing, false
	}
	t := en.Thresholds[i]
	return Threshold{KB: en.Ref(), ThresholdID: t.ID, Op: t.Op, Value: t.Value, Unit: t.Unit, Signoff: signoff(en)}, true
}

// param 取条目 kbID 的无条件参数点值；缺失返回 false。
func param(in *Input, kbID, id string) (float64, Threshold, bool) {
	missing := Threshold{KB: "kb:" + kbID, ThresholdID: id, Signoff: SignoffPending}
	if in.KB == nil {
		return 0, missing, false
	}
	en, ok := in.KB.Entry(kbID)
	if !ok {
		return 0, missing, false
	}
	p, ok := en.Param(id, nil)
	if !ok || p.Value == nil {
		return 0, missing, false
	}
	return *p.Value, Threshold{KB: en.Ref(), ThresholdID: p.ID, Value: *p.Value, Unit: p.Unit, Signoff: signoff(en)}, true
}

func signoff(en *knowledge.Entry) string {
	if en.ReviewStatus == reviewPending {
		return SignoffPending
	}
	return ""
}

// compare 按知识阈值 op 比较。
func compare(op string, v, th float64) bool {
	switch op {
	case ">=":
		return v >= th
	case ">":
		return v > th
	case "<=":
		return v <= th
	case "<":
		return v < th
	case "==":
		return v == th
	}
	return false
}

// observed 是来自工具 src 的实测值。
func observed(src Source, value any, unit string) Observed {
	o := Observed{Value: value, Unit: unit, Source: src.Source, ToolCallID: src.ToolCallID}
	if o.Source == "" {
		o.Source = src.Tool
	}
	if !src.CollectedAt.IsZero() {
		at := src.CollectedAt.UTC()
		o.CollectedAt = &at
	}
	return o
}

// source 取工具结果：未调用、非 OK、无数据或无 tool_call_id 时返回失败说明。
func source(in *Input, tool string) (Source, string) {
	src, ok := in.Sources[tool]
	switch {
	case !ok:
		return Source{Tool: tool}, tool + " not called"
	case src.Status != "OK" || src.Data == nil:
		return src, fmt.Sprintf("%s status %s without data", tool, src.Status)
	case src.ToolCallID == 0:
		return src, tool + " has no tool_call_id"
	}
	return src, ""
}

// maxClockSkew 是采集时刻允许超前于当前时刻的时钟偏差：超出即视为数据异常，不通过。
const maxClockSkew = 5 * time.Minute

// durationUnit 是时效阈值单位到时长的映射（未列出的单位判失败）。
var durationUnit = map[string]time.Duration{"h": time.Hour, "min": time.Minute, "s": time.Second}

// staleNote 按知识阈值判数据时效：阈值缺失、阈值单位与 unit 不符、采集时刻未知、超前当前时刻超过 maxClockSkew
// 或超时均返回说明（不通过）。
func staleNote(in *Input, src Source, kbID, id string, unit time.Duration) string {
	th, ok := threshold(in, kbID, id)
	if !ok {
		return "staleness threshold " + kbID + "#" + id + " missing"
	}
	if d, known := durationUnit[th.Unit]; !known || d != unit {
		return fmt.Sprintf("staleness threshold %s#%s unit %q, want %s", kbID, id, th.Unit, unit)
	}
	if src.CollectedAt.IsZero() {
		return src.Tool + " collected_at unknown"
	}
	if ahead := src.CollectedAt.Sub(in.Now); ahead > maxClockSkew {
		return fmt.Sprintf("%s collected_at %s ahead of now", src.Tool, ahead)
	}
	age := in.Now.Sub(src.CollectedAt).Seconds() / unit.Seconds()
	if !compare(th.Op, age, th.Value) {
		return fmt.Sprintf("%s stale: %.1f %s, need %s %g", src.Tool, age, th.Unit, th.Op, th.Value)
	}
	return ""
}

// physical 表示 v 是合法的非负物理量（风速、降雨、距离、电量、药量）：NaN / ±Inf / 负数均不合法。
func physical(v float64) bool { return v >= 0 && !math.IsInf(v, 1) }

// 逐日预报字段。
const (
	keyDaily = "daily"
	keyTime  = "time"
)

// forecastDays 取作业窗口覆盖的预报记录：有 daily 时按 in.Window 覆盖的本地日历日（From 所在日 … To 所在日，
// To 恰为零点时不含当日）选取，窗口为零值时取 daily[0]；无 daily 时返回顶层记录。daily 存在但窗口内无对应日返回说明。
func forecastDays(in *Input, data map[string]any) ([]map[string]any, string) {
	raw, ok := data[keyDaily]
	if !ok {
		return []map[string]any{data}, ""
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, "forecast daily empty"
	}
	if in.Window.From.IsZero() {
		d0, ok := list[0].(map[string]any)
		if !ok {
			return nil, "forecast daily[0] malformed"
		}
		return []map[string]any{d0}, ""
	}
	want := windowDays(in)
	var out []map[string]any
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := recordDate(m[keyTime]); ok && want[d] {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("forecast has no day in window %s .. %s",
			in.Window.From.In(in.Loc).Format(time.DateOnly), in.Window.To.In(in.Loc).Format(time.DateOnly))
	}
	return out, ""
}

// windowDays 是作业窗口覆盖的本地日历日集合（YYYY-MM-DD）。
func windowDays(in *Input) map[string]bool {
	first, last := day(in.Window.From, in.Loc), day(in.Window.From, in.Loc)
	if in.Window.To.After(in.Window.From) {
		last = day(in.Window.To.Add(-time.Nanosecond), in.Loc)
	}
	out := map[string]bool{}
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		out[d.Format(time.DateOnly)] = true
	}
	return out
}

// recordDate 取逐日记录的日期：YYYY-MM-DD 原样；RFC3339 取其自身时区下的日期（预报为当日 UTC 零点）。
func recordDate(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.Format(time.DateOnly), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format(time.DateOnly), true
	}
	return "", false
}

// number 按 keys 依次取记录中的第一个数值字段。
func number(rec map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := rec[k].(float64); ok {
			return v, true
		}
	}
	return 0, false
}
