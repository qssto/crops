package constraint

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// 地块档案（field.profile）中的空域与相邻地块字段。
const (
	keyNoFlyZones    = "no_fly_zones"
	keyAdjacent      = "adjacent_fields"
	keyZoneKind      = "kind"
	keyName          = "name"
	keyLabel         = "label"
	keyDistanceM     = "distance_m"
	keySensitive     = "sensitive"
	keyBearingDeg    = "bearing_deg"
	keyWindDirDeg    = "wind_dir_deg"
	zoneNoFly        = "no_fly"
	zoneSensitive    = "sensitive"
	unitMeter        = "m"
	fullCircleDeg    = 360.0
	halfCircleDeg    = 180.0
	payloadProductKy = "product"
)

// zoneBuffer 是空域类型对应的缓冲阈值 id。
var zoneBuffer = map[string]string{zoneNoFly: thNoFlyBuffer, zoneSensitive: thSensitiveBuf}

// noFlyChecker：禁飞区 / 敏感区。field.profile.no_fly_zones[] 每项 {name, kind, distance_m}（地块边界到区域边界的最近距离）；
// 字段缺失不通过、空列表通过、未知 kind 不通过。距离须严格大于缓冲（airspace#no-fly-buffer / sensitive-buffer）：
// 恰在边界上（distance_m == 缓冲）按「在区内」处理，不通过。观测值为余量（distance − buffer）最小一项的距离（米），
// 阈值为该项的缓冲，Reviewer 可按 op 直接复算。
type noFlyChecker struct{}

func (noFlyChecker) RuleID() string { return RuleNoFly }

func (noFlyChecker) Check(in *Input) Result {
	src, problem := source(in, ToolProfile)
	obs := observed(src, nil, unitMeter)
	nofly, okNoFly := threshold(in, KBAirspace, thNoFlyBuffer)
	if problem != "" {
		return fail(nofly, obs, problem)
	}
	raw, present := src.Data[keyNoFlyZones]
	zones, isList := raw.([]any)
	if !present || (raw != nil && !isList) {
		return fail(nofly, obs, "profile has no "+keyNoFlyZones)
	}
	if !okNoFly {
		return fail(nofly, obs, "threshold missing in knowledge")
	}
	worst, worstD, worstTh, worstNote, passed := math.Inf(1), 0.0, nofly, noteNoZone, true
	for _, z := range zones {
		m, _ := z.(map[string]any) // 非对象条目按缺字段处理
		kind, _ := m[keyZoneKind].(string)
		thID, known := zoneBuffer[kind]
		if !known {
			return fail(nofly, obs, fmt.Sprintf("unknown zone kind %q", kind))
		}
		th, ok := threshold(in, KBAirspace, thID)
		if !ok {
			return fail(th, obs, "threshold missing in knowledge")
		}
		d, ok := m[keyDistanceM].(float64)
		if !ok {
			return fail(th, obs, "zone without "+keyDistanceM)
		}
		if !physical(d) {
			return fail(th, obs, fmt.Sprintf("zone %s negative %s %g", kind, keyDistanceM, d))
		}
		name, _ := m[keyName].(string)
		passed = passed && compare(th.Op, d, th.Value)
		if margin := d - th.Value; margin < worst {
			worst, worstD, worstTh = margin, d, th
			worstNote = fmt.Sprintf("%s %s at %gm, margin %gm", kind, name, d, margin)
		}
	}
	if math.IsInf(worst, 1) {
		obs.Note = worstNote
		return Result{Passed: true, Observed: obs, Threshold: nofly}
	}
	obs.Value, obs.Note = worstD, worstNote+" (boundary counts as inside)"
	return Result{Passed: passed, Observed: obs, Threshold: worstTh}
}

// driftChecker：漂移风险。field.profile.adjacent_fields[] 每项 {label, crop, sensitive, bearing_deg, distance_m}；
// sensitive 须为布尔（缺失或非布尔不通过），仅敏感相邻地块参与：方位与下风向（风向 + 180°）的夹角满足
// drift#drift-sector-half-angle（op 与值来自知识，≤ 45° 含边界）即在下风向扇区内；取作业窗口内各预报日扇区内最近的一块，
// 其距离须满足漂移缓冲（无人机 drift-buffer-uav，其余 drift-buffer-knapsack，op 来自知识：> 缓冲），否则不通过。
// 有敏感相邻地块时 weather.forecast 必须给出 wind_dir_deg（风的来向），否则不通过。字段缺失不通过、空列表通过、距离为负不通过。
type driftChecker struct{}

func (driftChecker) RuleID() string { return RuleDrift }

func (driftChecker) Check(in *Input) Result {
	bufID := thDriftKnapsack
	if in.DeviceType == DeviceUAV {
		bufID = thDriftUAV
	}
	buf, okBuf := threshold(in, KBDrift, bufID)
	sector, okSector := threshold(in, KBDrift, thDriftSector)
	src, problem := source(in, ToolProfile)
	obs := observed(src, nil, unitMeter)
	switch {
	case !okBuf || !okSector:
		return fail(buf, obs, "threshold missing in knowledge")
	case problem != "":
		return fail(buf, obs, problem)
	}
	sensitive, note := sensitiveFields(src.Data)
	if note != "" {
		return fail(buf, obs, note)
	}
	if len(sensitive) == 0 {
		obs.Note = noteNoSensitive
		return Result{Passed: true, Observed: obs, Threshold: buf}
	}
	winds, note := windDirs(in)
	if note != "" {
		return fail(buf, obs, note)
	}
	nearest, note := math.Inf(1), noteNoDownwind
	for _, wind := range winds {
		downwind := math.Mod(wind+halfCircleDeg, fullCircleDeg)
		for _, m := range sensitive {
			bearing, okB := m[keyBearingDeg].(float64)
			d, okD := m[keyDistanceM].(float64)
			switch {
			case !okB || !okD:
				return fail(buf, obs, "adjacent field without "+keyBearingDeg+"/"+keyDistanceM)
			case !physical(d):
				return fail(buf, obs, fmt.Sprintf("adjacent field negative %s %g", keyDistanceM, d))
			}
			if !compare(sector.Op, angleDiff(bearing, downwind), sector.Value) || d >= nearest {
				continue
			}
			label, _ := m[keyLabel].(string) // 非字符串即无标签，只影响说明
			nearest, note = d, fmt.Sprintf("sensitive %s downwind at %gm (wind from %g°)", label, d, wind)
		}
	}
	obs.Note = note
	if math.IsInf(nearest, 1) {
		return Result{Passed: true, Observed: obs, Threshold: buf}
	}
	obs.Value = nearest
	return Result{Passed: compare(buf.Op, nearest, buf.Value), Observed: obs, Threshold: buf}
}

// sensitiveFields 取 adjacent_fields 中的敏感地块：字段缺失 / 非列表、条目非对象、sensitive 缺失或非布尔均返回说明。
func sensitiveFields(data map[string]any) ([]map[string]any, string) {
	raw, present := data[keyAdjacent]
	fields, isList := raw.([]any)
	if !present || (raw != nil && !isList) {
		return nil, "profile has no " + keyAdjacent
	}
	out := make([]map[string]any, 0, len(fields))
	for i, f := range fields {
		m, ok := f.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("%s[%d] is not an object", keyAdjacent, i)
		}
		s, ok := m[keySensitive].(bool)
		if !ok {
			return nil, fmt.Sprintf("%s[%d].%s is not a bool", keyAdjacent, i, keySensitive)
		}
		if s {
			out = append(out, m)
		}
	}
	return out, ""
}

// windDirs 取作业窗口内各预报日的风向（来向，度）：预报须 OK、带 tool_call_id、在时效内，且每一日都有 wind_dir_deg。
func windDirs(in *Input) ([]float64, string) {
	wsrc, problem := source(in, ToolForecast)
	if problem != "" {
		return nil, problem
	}
	if note := staleNote(in, wsrc, KBSpray, thWeatherStale, time.Hour); note != "" {
		return nil, note
	}
	recs, note := forecastDays(in, wsrc.Data)
	if note != "" {
		return nil, note
	}
	out := make([]float64, 0, len(recs))
	for _, rec := range recs {
		w, ok := number(rec, keyWindDirDeg)
		if !ok || math.IsNaN(w) || math.IsInf(w, 0) {
			return nil, "forecast has no " + keyWindDirDeg
		}
		out = append(out, w)
	}
	return out, ""
}

// angleDiff 是两个方位角的最小夹角（0–180°）。
func angleDiff(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), fullCircleDeg)
	if d > halfCircleDeg {
		d = fullCircleDeg - d
	}
	return d
}

// payloadProduct 取作业单 payload.product；不可解析即无药剂名。
func payloadProduct(raw json.RawMessage) string {
	var p map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return ""
	}
	s, _ := p[payloadProductKy].(string)
	return s
}
