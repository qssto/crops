package constraint

import (
	"fmt"
	"math"
	"time"
)

// device.status 字段（L2 设备 Adapter 输出）。
const (
	keyOnline     = "online"
	keyState      = "state"
	keyBatteryPct = "battery_pct"
	keyTankL      = "tank_l"
	keyLastSeenAt = "last_seen_at"
	keyDeviceID   = "device_id"
	unitPct       = "%"
	unitLiter     = "L"
)

// unusableState 是不可作业的设备状态。
var unusableState = map[string]bool{"fault": true, "offline": true}

// deviceUsable 判 device.status 可用：作业单 payload 带 device_id 且结果正是这台设备、OK、带 tool_call_id、在线、
// 非故障 / 离线、last_seen_at 在 uav-device#device-staleness（秒）内。返回失败说明（空串为可用）。
func deviceUsable(in *Input) (Source, string) {
	src, problem := source(in, ToolDevice)
	switch {
	case in.DeviceID == "":
		return src, "order payload has no " + keyDeviceID
	case problem != "":
		return src, problem
	}
	if id, _ := src.Data[keyDeviceID].(string); id != in.DeviceID { // 非字符串按不匹配处理
		return src, fmt.Sprintf("device.status is for %q, order device %q", id, in.DeviceID)
	}
	if online, _ := src.Data[keyOnline].(bool); !online {
		return src, "device offline"
	}
	if st, _ := src.Data[keyState].(string); unusableState[st] {
		return src, "device state " + st
	}
	seen, ok := parseTime(src.Data[keyLastSeenAt])
	if !ok {
		return src, "device last_seen_at unknown"
	}
	seenSrc := src
	seenSrc.CollectedAt = seen
	if note := staleNote(in, seenSrc, KBUAVDevice, thDeviceStale, time.Second); note != "" {
		return src, note
	}
	return src, ""
}

func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// sortieMu 是一次起降可作业的亩数：min(地块面积, 药箱容量 / 亩用水量)。
func sortieMu(in *Input) (float64, string) {
	if in.AreaMu == nil || *in.AreaMu <= 0 {
		return 0, "area_mu unknown"
	}
	tank, _, ok := param(in, KBUAVDevice, paramTankL)
	if !ok {
		return 0, "param " + paramTankL + " missing in knowledge"
	}
	vol, _, ok := param(in, KBUAVOp, paramSprayVolume)
	if !ok || vol <= 0 {
		return 0, "param " + paramSprayVolume + " missing in knowledge"
	}
	return math.Min(*in.AreaMu, tank/vol), ""
}

// batteryChecker（PRE_DISPATCH，无人机）：电量须满足 uav-device#battery-margin（op 来自知识）相对一架次所需电量
// = sortieMu / 满电作业亩数（uav_full_charge_mu）× 100% 的余量；行内 threshold.value = 所需电量 + 余量，可直接复算。
type batteryChecker struct{}

func (batteryChecker) RuleID() string { return RuleBattery }

func (batteryChecker) Check(in *Input) Result {
	th, okTh := threshold(in, KBUAVDevice, thBatteryMargin)
	full, _, okFull := param(in, KBUAVDevice, paramChargeMu)
	src, problem := deviceUsable(in)
	obs := observed(src, nil, unitPct)
	switch {
	case !okTh:
		return fail(th, obs, "threshold missing in knowledge")
	case !okFull || full <= 0:
		return fail(th, obs, "param "+paramChargeMu+" missing in knowledge")
	case problem != "":
		return fail(th, obs, problem)
	}
	mu, note := sortieMu(in)
	if note != "" {
		return fail(th, obs, note)
	}
	pct, ok := src.Data[keyBatteryPct].(float64)
	if !ok {
		return fail(th, obs, "device.status has no "+keyBatteryPct)
	}
	obs.Value = pct
	if !physical(pct) {
		return fail(th, obs, "device.status negative "+keyBatteryPct)
	}
	need := math.Ceil(mu / full * 100)
	th.Value, th.Unit = need+th.Value, unitPct
	obs.Note = fmt.Sprintf("sortie %.2f mu / full charge %g mu = %g%%", mu, full, need)
	return Result{Passed: compare(th.Op, pct, th.Value), Observed: obs, Threshold: th}
}

// payloadChecker（PRE_DISPATCH，无人机）：药箱 tank_l 须满足 uav-device#payload-margin（op 来自知识）相对一架次药液量
// = sortieMu × 亩用水量的余量；行内 threshold.value = 所需药液量 + 余量。
type payloadChecker struct{}

func (payloadChecker) RuleID() string { return RulePayload }

func (payloadChecker) Check(in *Input) Result {
	th, okTh := threshold(in, KBUAVDevice, thPayloadMargin)
	vol, _, okVol := param(in, KBUAVOp, paramSprayVolume)
	src, problem := deviceUsable(in)
	obs := observed(src, nil, unitLiter)
	switch {
	case !okTh:
		return fail(th, obs, "threshold missing in knowledge")
	case !okVol || vol <= 0:
		return fail(th, obs, "param "+paramSprayVolume+" missing in knowledge")
	case problem != "":
		return fail(th, obs, problem)
	}
	mu, note := sortieMu(in)
	if note != "" {
		return fail(th, obs, note)
	}
	tank, ok := src.Data[keyTankL].(float64)
	if !ok {
		return fail(th, obs, "device.status has no "+keyTankL)
	}
	obs.Value = tank
	if !physical(tank) {
		return fail(th, obs, "device.status negative "+keyTankL)
	}
	need := math.Round(mu*vol*100) / 100
	th.Value, th.Unit = need+th.Value, unitLiter
	obs.Note = fmt.Sprintf("sortie %.2f mu × %g L/mu = %g L", mu, vol, need)
	return Result{Passed: compare(th.Op, tank, th.Value), Observed: obs, Threshold: th}
}
