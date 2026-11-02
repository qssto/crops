package constraint

import (
	"fmt"
	"math"
	"time"

	"agri-steward/internal/model"
)

// weatherChecker 判一个天气指标：预报须 OK、带 tool_call_id、在时效内（spray#weather-staleness），
// 且作业窗口覆盖的每个预报日都有数值；取窗口内最不利值（上限类取最大、下限类取最小）按知识阈值 op 比较。
// pick 按作业类型 / 设备选阈值所在条目与阈值 id；nonNegative 的指标（风速、降雨）为负数即不通过。
type weatherChecker struct {
	rule        string
	keys        []string // 预报字段名（含别名，按优先级）
	nonNegative bool
	pick        func(in *Input) (kbID, thID string)
}

func (c weatherChecker) RuleID() string { return c.rule }

func (c weatherChecker) Check(in *Input) Result {
	kbID, thID := c.pick(in)
	th, ok := threshold(in, kbID, thID)
	src, problem := source(in, ToolForecast)
	obs := observed(src, nil, th.Unit)
	switch {
	case !ok:
		return fail(th, obs, "threshold missing in knowledge")
	case problem != "":
		return fail(th, obs, problem)
	}
	if note := staleNote(in, src, KBSpray, thWeatherStale, time.Hour); note != "" {
		return fail(th, obs, note)
	}
	recs, note := forecastDays(in, src.Data)
	if note != "" {
		return fail(th, obs, note)
	}
	var worst float64
	for i, rec := range recs {
		v, ok := number(rec, c.keys...)
		switch {
		case !ok || math.IsNaN(v) || math.IsInf(v, 0):
			return fail(th, obs, fmt.Sprintf("forecast has no %v", c.keys))
		case c.nonNegative && !physical(v):
			obs.Value = v
			return fail(th, obs, fmt.Sprintf("forecast %v negative", c.keys))
		}
		if i == 0 || worse(th.Op, v, worst) {
			worst = v
		}
	}
	obs.Value = worst
	if len(recs) > 1 {
		obs.Note = fmt.Sprintf("worst of %d forecast days in window", len(recs))
	}
	return Result{Passed: compare(th.Op, worst, th.Value), Observed: obs, Threshold: th}
}

// worse 表示在阈值 op 下 v 比 cur 更不利：上限类（< / <=）取大，下限类（> / >=）取小。
func worse(op string, v, cur float64) bool {
	switch op {
	case "<", "<=":
		return v > cur
	case ">", ">=":
		return v < cur
	}
	return false
}

// windChecker：无人机取 wind-uav（≤ 3.0 m/s），其余取 wind-knapsack（≤ 5.4 m/s）。
func windChecker() Checker {
	return weatherChecker{rule: RuleWind, keys: []string{"wind_speed_ms"}, nonNegative: true, pick: func(in *Input) (string, string) {
		if in.DeviceType == DeviceUAV {
			return KBSpray, thWindUAV
		}
		return KBSpray, thWindKnapsack
	}}
}

// rainChecker：人工撒施追肥取 fertilize#rain-24h（< 20 mm），喷雾类取 spray#rain-24h（< 5 mm）。
func rainChecker() Checker {
	return weatherChecker{rule: RuleRain, keys: []string{"rain_mm_24h", "rain_mm"}, nonNegative: true, pick: func(in *Input) (string, string) {
		if in.WorkType == model.WorkFertilize && !sprayDevice(in.DeviceType) {
			return KBFertilize, thRain24h
		}
		return KBSpray, thRain24h
	}}
}

// tempChecker：喷雾类气温上限 spray#temp-max（≤ 35 ℃）。优先取日最高温 temp_max_c；真实 weather.forecast
// 逐日只有日均温 temp_c，退而取之（偏乐观，已记 dev-log 限制）。
func tempChecker() Checker {
	return weatherChecker{rule: RuleTemp, keys: []string{"temp_max_c", "temp_c"}, pick: func(*Input) (string, string) {
		return KBSpray, thTempMax
	}}
}

// sprayDevice 表示喷雾类设备（背负式 / 无人机）。
func sprayDevice(d string) bool { return d == DeviceKnapsack || d == DeviceUAV }
