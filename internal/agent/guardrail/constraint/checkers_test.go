package constraint

import (
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

func forecast(kv ...any) Source {
	d := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i].(string)] = kv[i+1]
	}
	return ok(ToolForecast, d)
}

func TestWindChecker(t *testing.T) {
	stale := forecast("wind_speed_ms", 1.0)
	stale.CollectedAt = testNow.Add(-6*time.Hour - time.Minute)
	unknownAt := forecast("wind_speed_ms", 1.0)
	unknownAt.CollectedAt = time.Time{}
	noID := forecast("wind_speed_ms", 1.0)
	noID.ToolCallID = 0
	failed := forecast("wind_speed_ms", 1.0)
	failed.Status = "TIMEOUT"
	runCases(t, windChecker(), []checkCase{
		{name: "knapsack normal", in: input(t, with(forecast("wind_speed_ms", 3.0))), want: true, value: 3.0},
		{name: "knapsack boundary 5.4 passes", in: input(t, with(forecast("wind_speed_ms", 5.4))), want: true},
		{name: "knapsack over", in: input(t, with(forecast("wind_speed_ms", 5.5))), want: false, value: 5.5},
		{name: "uav boundary 3.0 passes", in: input(t, device(DeviceUAV), with(forecast("wind_speed_ms", 3.0))), want: true},
		{name: "uav over", in: input(t, device(DeviceUAV), with(forecast("wind_speed_ms", 3.1))), want: false},
		{name: "daily[0] fallback", in: input(t, with(forecast("daily", []any{map[string]any{"wind_speed_ms": 2.0}}))), want: true},
		{name: "forecast not called", in: input(t), want: false},
		{name: "forecast failed", in: input(t, with(failed)), want: false},
		{name: "no tool_call_id", in: input(t, with(noID)), want: false},
		{name: "stale forecast", in: input(t, with(stale)), want: false},
		{name: "collected_at unknown", in: input(t, with(unknownAt)), want: false},
		{name: "value missing", in: input(t, with(forecast("temp_c", 20.0))), want: false},
	})
}

func TestRainChecker(t *testing.T) {
	manual := func(in *Input) { in.WorkType, in.DeviceType = model.WorkFertilize, DeviceManual }
	runCases(t, rainChecker(), []checkCase{
		{name: "spray normal", in: input(t, with(forecast("rain_mm_24h", 1.0))), want: true},
		{name: "spray boundary 5 fails", in: input(t, with(forecast("rain_mm_24h", 5.0))), want: false},
		{name: "alias rain_mm", in: input(t, with(forecast("rain_mm", 4.9))), want: true},
		{name: "manual fertilize 19.9", in: input(t, manual, with(forecast("rain_mm_24h", 19.9))), want: true},
		{name: "manual fertilize boundary 20 fails", in: input(t, manual, with(forecast("rain_mm_24h", 20.0))), want: false},
		{name: "missing", in: input(t, manual), want: false},
	})
}

func TestTempChecker(t *testing.T) {
	runCases(t, tempChecker(), []checkCase{
		{name: "normal", in: input(t, with(forecast("temp_c", 26.0))), want: true},
		{name: "boundary 35 passes", in: input(t, with(forecast("temp_c", 35.0))), want: true},
		{name: "over", in: input(t, with(forecast("temp_c", 35.1))), want: false},
		{name: "missing", in: input(t, with(forecast("wind_speed_ms", 1.0))), want: false},
	})
}

// daily 是逐日预报：每项 {time: 当日 UTC 零点, kv...}。
func daily(days ...map[string]any) Source { return forecast("daily", toAny(days)) }

func toAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

func dayRec(d time.Time, kv ...any) map[string]any {
	m := map[string]any{"time": time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// window 是作业窗口 [now, now+h)。
func window(h int) func(*Input) {
	return func(in *Input) {
		in.Window = model.TimeWindow{From: testNow, To: testNow.Add(time.Duration(h) * time.Hour)}
	}
}

// TestWeatherWindow（审查 10）：逐日预报按作业窗口覆盖的本地日历日取最不利值；窗口外的日子不参与；窗口内无记录不通过。
func TestWeatherWindow(t *testing.T) {
	tomorrow := today.AddDate(0, 0, 1)
	twoDays := daily(dayRec(today, "wind_speed_ms", 2.0, "rain_mm", 0.0, "temp_c", 26.0),
		dayRec(tomorrow, "wind_speed_ms", 6.0, "rain_mm", 8.0, "temp_c", 36.0),
		dayRec(today.AddDate(0, 0, 2), "wind_speed_ms", 9.0))
	runCases(t, windChecker(), []checkCase{
		{name: "tomorrow in 24h window is worst", in: input(t, window(24), with(twoDays)), want: false, value: 6.0},
		{name: "same-day window only today", in: input(t, window(2), with(twoDays)), want: true, value: 2.0},
		{name: "zero window takes daily[0]", in: input(t, with(twoDays)), want: true, value: 2.0},
		{name: "no day in window fails", in: input(t, window(24), with(daily(dayRec(today.AddDate(0, 0, 5), "wind_speed_ms", 1.0)))), want: false},
		{name: "window day without value fails", in: input(t, window(24), with(daily(dayRec(today, "wind_speed_ms", 1.0),
			dayRec(tomorrow, "temp_c", 20.0)))), want: false},
	})
	runCases(t, rainChecker(), []checkCase{
		{name: "rain tomorrow blocks", in: input(t, window(24), with(twoDays)), want: false, value: 8.0},
	})
	runCases(t, tempChecker(), []checkCase{
		{name: "temp tomorrow blocks", in: input(t, window(24), with(twoDays)), want: false, value: 36.0},
		{name: "temp_max_c preferred over mean", in: input(t, with(forecast("temp_c", 30.0, "temp_max_c", 36.0))), want: false, value: 36.0},
	})
}

// TestPhysicalAndFreshness（审查 8 / 9）：负的物理量、超前当前时刻超过时钟偏差的采集时刻、阈值单位不符均不通过。
func TestPhysicalAndFreshness(t *testing.T) {
	future := forecast("wind_speed_ms", 1.0)
	future.CollectedAt = testNow.Add(maxClockSkew + time.Second)
	skewOK := forecast("wind_speed_ms", 1.0)
	skewOK.CollectedAt = testNow.Add(maxClockSkew)
	runCases(t, windChecker(), []checkCase{
		{name: "negative wind fails", in: input(t, with(forecast("wind_speed_ms", -0.1))), want: false},
		{name: "future collected_at beyond skew fails", in: input(t, with(future)), want: false},
		{name: "future collected_at within skew passes", in: input(t, with(skewOK)), want: true},
	})
	runCases(t, rainChecker(), []checkCase{
		{name: "negative rain fails", in: input(t, with(forecast("rain_mm_24h", -1.0))), want: false},
	})
	runCases(t, tempChecker(), []checkCase{
		{name: "sub-zero temperature is valid", in: input(t, with(forecast("temp_c", -3.0))), want: true},
	})
	t.Run("staleness unit mismatch fails", func(t *testing.T) {
		in := input(t, with(forecast("wind_speed_ms", 1.0)))
		in.KB = patchedKB{base: repoKB(t), kbID: KBSpray, thID: thWeatherStale, unit: "s"}
		if got := windChecker().Check(in); got.Passed || !strings.Contains(got.Observed.Note, "unit") {
			t.Fatalf("got %+v", got)
		}
	})
}

// patchedKB 改写某条阈值的单位（验证单位不符失败关闭）。
type patchedKB struct {
	base             KB
	kbID, thID, unit string
}

func (p patchedKB) Entry(id string) (*knowledge.Entry, bool) {
	en, ok := p.base.Entry(id)
	if !ok || id != p.kbID {
		return en, ok
	}
	cp := *en
	cp.Thresholds = slices.Clone(en.Thresholds)
	for i := range cp.Thresholds {
		if cp.Thresholds[i].ID == p.thID {
			cp.Thresholds[i].Unit = p.unit
		}
	}
	return &cp, true
}

func TestThresholdMissingInKnowledge(t *testing.T) {
	in := input(t, with(forecast("wind_speed_ms", 1.0)))
	in.KB = nil
	got := windChecker().Check(in)
	if got.Passed || got.Threshold.KB != "kb:"+KBSpray || got.Threshold.Signoff != SignoffPending {
		t.Fatalf("got %+v", got)
	}
}

func product(phi *int) func(*Input) {
	return func(in *Input) {
		in.Product = &Product{ID: "p", CommonName: "吡虫啉", Registered: true, PHIDays: phi, Ref: "kb:common/pesticide_registry@v1"}
	}
}

// phiOf：吡虫啉 14 天，噻虫嗪 21 天，噻呋酰胺 30 天，五氟磺草胺已登记但 PHI 不适用，其余未知。
func phiOf(in *Input) {
	in.PHIOf = func(name string) (*int, bool) {
		switch name {
		case "吡虫啉":
			return intp(14), true
		case "噻虫嗪":
			return intp(21), true
		case "噻呋酰胺":
			return intp(30), true
		case "五氟磺草胺":
			return nil, true
		}
		return nil, false
	}
}

func harvest(d time.Time) Source { return ok(ToolProfile, map[string]any{"expected_harvest": ymd(d)}) }

func history(data map[string]any) Source { return ok(ToolHistory, data) }

func TestPHIChecker(t *testing.T) {
	sprayAgo := func(n int, name string) map[string]any {
		return map[string]any{"operations": []any{map[string]any{"date": ymd(today.AddDate(0, 0, -n)), "kind": "spray", "product": name}}}
	}
	noHist := history(map[string]any{"operations": []any{}})
	notLoaded := func(in *Input) { in.LocalLoaded = false }
	// 农事记录回写的 order:9 与本地单 9 是同一作业：去重后只按本地单（噻虫嗪，2 天前完成）计。
	dupHist := history(map[string]any{"operations": []any{map[string]any{"date": ymd(today.AddDate(0, 0, -2)),
		"kind": "spray", "product": "未知药", "order_id": 9.0}}})
	runCases(t, phiChecker{}, []checkCase{
		{name: "margin 0 passes", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 14)), noHist)), want: true, value: 0},
		{name: "off by one day fails", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 13)), noHist)), want: false, value: -1},
		{name: "phenology fallback", in: input(t, product(intp(14)), phiOf,
			with(ok(ToolPhenology, map[string]any{"expected_harvest": ymd(today.AddDate(0, 0, 30))}), noHist)), want: true, value: 16},
		{name: "history spray extends earliest", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 15)),
			history(sprayAgo(5, "噻虫嗪")))), want: false, value: -1},
		{name: "history unknown product fails", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 15)),
			history(sprayAgo(5, "未知药")))), want: false},
		{name: "history spray without product fails", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 15)),
			history(map[string]any{"last_spray": map[string]any{"date": ymd(today.AddDate(0, 0, -5))}}))), want: false},
		{name: "local spray without product fails", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 30)), noHist),
			local(localOrder(9, model.WorkSpray, model.OrderDone, testNow.AddDate(0, 0, -2), ""))), want: false},
		{name: "history other kind without product ignored", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 15)),
			history(map[string]any{"last_fertilize": map[string]any{"date": ymd(today.AddDate(0, 0, -5))}}))), want: true, value: 1},
		{name: "history missing fails", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 30)))), want: false},
		{name: "local orders not loaded fails", in: input(t, product(intp(14)), phiOf, notLoaded,
			with(harvest(today.AddDate(0, 0, 30)), noHist)), want: false},
		{name: "in-flight local spray counts as today", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 20)), noHist),
			local(localOrder(9, model.WorkSpray, model.OrderApproved, testNow.AddDate(0, 0, -10), "噻虫嗪"))), want: false, value: -1},
		{name: "done local spray by updated_at", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 20)), noHist),
			local(localOrder(9, model.WorkSpray, model.OrderDone, testNow.AddDate(0, 0, -2), "噻虫嗪"))), want: true, value: 1},
		{name: "history duplicate of local order deduped", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 20)), dupHist),
			local(localOrder(9, model.WorkSpray, model.OrderDone, testNow.AddDate(0, 0, -2), "噻虫嗪"))), want: true, value: 1},
		{name: "phi not applicable", in: input(t, product(nil), phiOf, with(harvest(today), noHist)), want: true, value: 0},
		// 复核第 20 条：本药 PHI 为空不短路，5 天前用过噻呋酰胺（30 天）、20 天后收获 → 余量 −5
		{name: "phi not applicable still checks history", in: input(t, product(nil), phiOf, with(harvest(today.AddDate(0, 0, 20)),
			history(sprayAgo(5, "噻呋酰胺")))), want: false, value: -5},
		{name: "phi not applicable history unknown fails", in: input(t, product(nil), phiOf, with(harvest(today.AddDate(0, 0, 20)),
			history(sprayAgo(5, "未知药")))), want: false},
		{name: "phi not applicable history not loaded fails", in: input(t, product(nil), phiOf, notLoaded,
			with(harvest(today.AddDate(0, 0, 20)), noHist)), want: false},
		// 复核第 19 条：历史里已登记但本作物 PHI 为空的药（五氟磺草胺）不计入最早收获日，也不误拦
		{name: "history phi not applicable ignored", in: input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 15)),
			history(sprayAgo(5, "五氟磺草胺")))), want: true, value: 1},
		{name: "no product", in: input(t, with(harvest(today.AddDate(0, 0, 30)))), want: false},
		{name: "harvest missing", in: input(t, product(intp(14)), with(ok(ToolProfile, map[string]any{"crop": "rice"}))), want: false},
		{name: "profile not called", in: input(t, product(intp(14))), want: false},
	})
	t.Run("note names phi not applicable history", func(t *testing.T) {
		got := phiChecker{}.Check(input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 30)), history(sprayAgo(5, "五氟磺草胺")))))
		if !got.Passed || !strings.Contains(got.Observed.Note, "phi not applicable: 五氟磺草胺@") {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("note names unrecognized product", func(t *testing.T) {
		got := phiChecker{}.Check(input(t, product(intp(14)), phiOf, with(harvest(today.AddDate(0, 0, 30)), history(sprayAgo(5, "未知药")))))
		if got.Passed || !strings.Contains(got.Observed.Note, "未知药") {
			t.Fatalf("got %+v", got)
		}
	})
}

// TestDaysCalendar：日历日差按农场时区的年月日计，跨夏令时切换（23 / 25 小时的一天）不少算一天。
func TestDaysCalendar(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	from := time.Date(2026, 3, 7, 0, 0, 0, 0, ny) // 3 月 8 日切夏令时，当天只有 23 小时
	if got := days(from, from.AddDate(0, 0, 2)); got != 2 {
		t.Fatalf("days across DST = %d, want 2", got)
	}
	if got := days(day(testNow, testLoc), day(testNow, testLoc).AddDate(0, 0, -3)); got != -3 {
		t.Fatalf("days backwards = %d, want -3", got)
	}
}

func TestRepeatChecker(t *testing.T) {
	ops := func(kind string, n int) Source {
		return history(map[string]any{"operations": []any{map[string]any{"date": ymd(today.AddDate(0, 0, -n)), "kind": kind}}})
	}
	empty := history(map[string]any{"operations": []any{}})
	notLoaded := func(in *Input) { in.LocalLoaded = false }
	runCases(t, repeatChecker{}, []checkCase{
		{name: "no previous spray", in: input(t, with(empty)), want: true},
		{name: "spray boundary 3 days passes", in: input(t, with(ops("spray", 3))), want: true, value: 3},
		{name: "spray 2 days fails", in: input(t, with(ops("spray", 2))), want: false, value: 2},
		{name: "last_spray field", in: input(t, with(history(map[string]any{"last_spray": map[string]any{"date": ymd(today.AddDate(0, 0, -1))}}))), want: false},
		{name: "other kind ignored", in: input(t, with(ops("fertilize", 0))), want: true},
		{name: "in-flight local with empty history", in: input(t, with(empty),
			local(localOrder(5, model.WorkSpray, model.OrderDispatched, testNow.AddDate(0, 0, -9), "吡虫啉"))), want: false, value: 0},
		{name: "done local 1 day ago", in: input(t, with(empty),
			local(localOrder(5, model.WorkSpray, model.OrderPartiallyDone, testNow.AddDate(0, 0, -1), ""))), want: false, value: 1},
		{name: "rejected local ignored", in: input(t, with(empty),
			local(localOrder(5, model.WorkSpray, model.OrderRejected, testNow, ""))), want: true},
		{name: "history copy of local order deduped", in: input(t, with(history(map[string]any{"operations": []any{
			map[string]any{"date": ymd(today.AddDate(0, 0, -1)), "kind": "spray", "order_id": 5.0}}})),
			local(localOrder(5, model.WorkSpray, model.OrderDone, testNow.AddDate(0, 0, -4), ""))), want: true, value: 4},
		{name: "fertilize 6 days fails", in: input(t, work(model.WorkFertilize), with(history(map[string]any{
			"last_fertilize": map[string]any{"date": ymd(today.AddDate(0, 0, -6))}}))), want: false, value: 6},
		{name: "fertilize boundary 7 passes", in: input(t, work(model.WorkFertilize), with(ops("fertilize", 7))), want: true},
		{name: "irrigate same day fails", in: input(t, work(model.WorkIrrigate), with(ops("irrigate", 0))), want: false},
		{name: "irrigate yesterday passes", in: input(t, work(model.WorkIrrigate), with(ops("irrigate", 1))), want: true},
		{name: "history missing", in: input(t), want: false},
		{name: "local orders not loaded", in: input(t, notLoaded, with(empty)), want: false},
	})
	t.Run("unknown work type", func(t *testing.T) {
		if got := (repeatChecker{}).Check(input(t, work("PRUNE"), with(empty))); got.Passed {
			t.Fatalf("got %+v", got)
		}
	})
}
