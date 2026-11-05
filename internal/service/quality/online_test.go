package quality

import (
	"testing"
	"time"

	"agri-steward/internal/model"
)

// 时区边界用注入时钟：业务日按农场时区当地零点切，窗口以 UTC 存。
func TestFarmDay_TimezoneBoundary(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		tz       string
		date     string
		from, to time.Time
	}{
		{name: "shanghai just after local midnight", now: time.Date(2026, 11, 3, 16, 0, 1, 0, time.UTC), tz: "Asia/Shanghai",
			date: "2026-11-03", from: time.Date(2026, 11, 2, 16, 0, 0, 0, time.UTC), to: time.Date(2026, 11, 3, 16, 0, 0, 0, time.UTC)},
		{name: "shanghai just before local midnight", now: time.Date(2026, 11, 3, 15, 59, 59, 0, time.UTC), tz: "Asia/Shanghai",
			date: "2026-11-02", from: time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC), to: time.Date(2026, 11, 2, 16, 0, 0, 0, time.UTC)},
		{name: "utc farm", now: time.Date(2026, 11, 3, 16, 0, 1, 0, time.UTC), tz: "UTC",
			date: "2026-11-02", from: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), to: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)},
		// 美东 2026-11-01 结束夏令时：该日 25 小时。
		{name: "dst fall back day is 25h", now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), tz: "America/New_York",
			date: "2026-11-01", from: time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), to: time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := FarmDay(tt.now, tt.tz)
			if err != nil {
				t.Fatal(err)
			}
			if d.Date != tt.date || !d.From.Equal(tt.from) || !d.To.Equal(tt.to) {
				t.Fatalf("FarmDay = %+v, want %s [%s, %s)", d, tt.date, tt.from, tt.to)
			}
		})
	}
	for _, tz := range []string{"", "Mars/Olympus"} {
		if _, err := FarmDay(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), tz); err == nil {
			t.Fatalf("FarmDay(%q) err = nil, want error", tz)
		}
	}
}

func TestFactState(t *testing.T) {
	tests := []struct {
		name     string
		j        JudgeCounts
		unjudged bool
		want     FactStatus
	}{
		{"unjudged wins", JudgeCounts{Scored: 10, Top: 9}, true, FactUnjudged},
		{"no sample", JudgeCounts{}, false, FactNoSample},
		{"unknown 10% is ok", JudgeCounts{Scored: 9, Top: 9, Unknown: 1}, false, FactOK},
		{"unknown over 10% undeterminable", JudgeCounts{Scored: 8, Top: 8, Unknown: 2}, false, FactUndeterminable},
		{"all unknown", JudgeCounts{Unknown: 3}, false, FactUndeterminable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FactState(tt.j, tt.unjudged); got != tt.want {
				t.Fatalf("FactState = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestOnlineRows(t *testing.T) {
	c := AdviceCounts{Total: 10, Advice: 6, Cited: 5, Refused: 4, ValidationFailed: 2, Degraded: 1}
	j := JudgeCounts{Scored: 4, Top: 3, Unknown: 0}
	got := map[string]model.QualityDaily{}
	for _, r := range OnlineRows(1, 2, "2026-11-03", c, j, FactOK) {
		got[r.Metric] = r
		if r.Source != model.MetricSourceOnline || r.SliceKey != model.SliceAll || r.TenantID != 1 || r.FarmID != 2 ||
			r.Low == nil || r.High == nil || *r.Low > r.Point || *r.High < r.Point {
			t.Fatalf("bad row %+v", r)
		}
	}
	want := map[string][3]int{ // k, n, estimate
		model.MetricCitationCoverage: {5, 6, 0}, model.MetricNumericAccuracy: {6, 8, 0},
		model.MetricRefusalRate: {4, 10, 0}, model.MetricFactConsistency: {3, 4, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %d metrics", got, len(want))
	}
	for m, w := range want {
		r := got[m]
		if r.Numerator != w[0] || r.Denominator != w[1] || r.IsEstimate != (w[2] == 1) {
			t.Fatalf("%s = %d/%d est=%v, want %v", m, r.Numerator, r.Denominator, r.IsEstimate, w)
		}
	}
	// 不可判 / 未评不写事实一致率；分母为 0 的指标不写。
	for _, fact := range []FactStatus{FactUnjudged, FactUndeterminable, FactNoSample} {
		for _, r := range OnlineRows(1, 2, "2026-11-03", c, j, fact) {
			if r.Metric == model.MetricFactConsistency {
				t.Fatalf("%s wrote fact_consistency", fact)
			}
		}
	}
	if rows := OnlineRows(1, 2, "2026-11-03", AdviceCounts{}, JudgeCounts{}, FactNoSample); len(rows) != 0 {
		t.Fatalf("empty day rows = %v, want none", rows)
	}
}
