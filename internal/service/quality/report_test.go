package quality

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"agri-steward/internal/model"
)

func offMetric(metric, slice string, point float64, samples int) model.OfflineMetric {
	return model.OfflineMetric{Metric: metric, Title: metricTitles[metric], Slice: slice, Status: model.MetricStatusOK,
		Interval: model.IntervalWilson, Samples: samples, Point: point, Low: point - 0.05, High: point + 0.05,
		Numerator: int(point * float64(samples)), Denominator: samples}
}

func synthOffline(expID uint64, ms ...model.OfflineMetric) *model.OfflineMetrics {
	return &model.OfflineMetrics{ExperimentID: expID, Seed: 20260101, Resamples: 1000, Metrics: ms}
}

// 验收 3：日报异常值 2 次点击内到 run_id —— 离线单元格链到 {exp}/report.html#sample-{id}（下钻页该锚点列出 run_id），
// 线上单元格直接链到 /runs/{run_id}。
func TestReport_LinksReachRunWithinTwoClicks(t *testing.T) {
	cur := offMetric(model.MetricCitationCoverage, model.SliceAll, 0.9, 50)
	cur.Failing = []string{"protect-007@v2", "protect-011@v1"}
	in := ReportInput{Date: "2026-11-03", Offline: Offline{State: OfflineOK, Latest: model.LatestNightly{ExperimentID: 42},
		Metrics: synthOffline(42, cur)}, OnlineFact: FactOK,
		Online: []model.QualityDaily{{Source: model.MetricSourceOnline, SliceKey: model.SliceAll, Metric: model.MetricCitationCoverage,
			Numerator: 7, Denominator: 8, Point: 0.875, Low: ptr(0.5), High: ptr(0.98)}},
		OnlineRuns: map[string][]uint64{model.MetricCitationCoverage: {101, 205}}}
	rep := BuildReport(in)
	html, err := RenderReport(rep)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	for _, want := range []string{`href="../../42/report.html#sample-protect-007"`, `href="/runs/101"`, `href="/runs/205"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("report missing link %s", want)
		}
	}
	row := rep.Rows[0]
	if row.Metric != model.MetricCitationCoverage || row.Online.Link != "/runs/101" {
		t.Fatalf("online cell link = %q", row.Online.Link)
	}
	// 只含 ID 与聚合值：没有结论原文、农场名等明细。
	if strings.Contains(page, "结论") {
		t.Fatal("report leaks card text")
	}
}

func TestReport_OfflineStates(t *testing.T) {
	tests := []struct {
		name string
		off  Offline
		want string
	}{
		{"void", Offline{State: OfflineVoid, Latest: model.LatestNightly{ExperimentID: 9, Void: true}}, TextVoid},
		{"no data", Offline{State: OfflineNoData}, TextNoData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := BuildReport(ReportInput{Date: "2026-11-03", Offline: tt.off})
			for _, r := range rep.Rows {
				if r.Offline.Text != tt.want || r.Offline.Value {
					t.Fatalf("%s offline = %+v, want %s", r.Metric, r.Offline, tt.want)
				}
			}
			if len(rep.Slices) != 0 {
				t.Fatalf("slices = %v, want none", rep.Slices)
			}
		})
	}
	// 单指标 UNDETERMINABLE：「不可判」；judge 未校准：不与基线做差值。
	und := offMetric(model.MetricFactConsistency, model.SliceAll, 0, 0)
	und.Status, und.UnknownShare, und.Judge = model.MetricStatusUndeterminable, 0.2, true
	hal := offMetric("hallucination_rate", model.SliceAll, 0.1, 50)
	hal.Judge = true
	base := offMetric("hallucination_rate", model.SliceAll, 0.2, 50)
	base.Judge = true
	rep := BuildReport(ReportInput{Offline: Offline{State: OfflineOK, Metrics: synthOffline(1, und, hal)},
		Baseline: synthOffline(0, base)})
	cells := map[string]Cell{}
	for _, r := range rep.Rows {
		cells[r.Metric] = r.Offline
	}
	if c := cells[model.MetricFactConsistency]; c.Text != TextUndeterminable {
		t.Fatalf("undeterminable cell = %+v", c)
	}
	if c := cells["hallucination_rate"]; c.Diff != TextNoDiff || c.Note != TextUncalibrated {
		t.Fatalf("uncalibrated cell = %+v, want no diff and 未校准", c)
	}
}

// 切片标红前做 Holm 校正：单看 p<0.05 的变差切片，校正后不显著则不标红；显著变差标红；显著变好不标红。
func TestReport_SliceHolm(t *testing.T) {
	m := model.MetricCitationCoverage
	cur := synthOffline(2,
		offMetric(m, model.SliceAll, 0.9, 200),
		offMetric(m, "category=protect", 0.50, 200),   // 大幅变差：Holm 后仍显著 → 红
		offMetric(m, "category=fertilize", 0.84, 200), // 小幅变差：单检 p≈0.04，Holm 后不显著 → 不红
		offMetric(m, "difficulty=hard", 0.99, 200),    // 变好：不红
		offMetric(m, "difficulty=easy", 0.90, 200),    // 不变
	)
	base := synthOffline(1,
		offMetric(m, model.SliceAll, 0.9, 200),
		offMetric(m, "category=protect", 0.90, 200),
		offMetric(m, "category=fertilize", 0.91, 200),
		offMetric(m, "difficulty=hard", 0.80, 200),
		offMetric(m, "difficulty=easy", 0.90, 200),
	)
	rep := BuildReport(ReportInput{Offline: Offline{State: OfflineOK, Metrics: cur}, Baseline: base})
	red := map[string]bool{}
	ps := map[string]string{}
	for _, s := range rep.Slices {
		red[s.Slice], ps[s.Slice] = s.Cell.Red, s.P
	}
	if !red["category=protect"] || red["category=fertilize"] || red["difficulty=hard"] || red["difficulty=easy"] {
		t.Fatalf("red = %v (p = %v)", red, ps)
	}
	if p := ps["category=fertilize"]; p >= "0.050" {
		t.Fatalf("fertilize raw p = %s, test needs it < 0.05 to exercise Holm", p)
	}
	if _, ok := red[model.SliceAll]; ok {
		t.Fatal("all slice listed in slice table")
	}
}

func TestUploadReport_KeyAndOffline(t *testing.T) {
	obj := &memObjects{}
	latest, err := json.Marshal(model.LatestNightly{ExperimentID: 9, Void: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := obj.PutObject(context.Background(), "eval", model.LatestNightlyKey, latest, ""); err != nil {
		t.Fatal(err)
	}
	off, err := LoadOffline(t.Context(), obj, "eval")
	if err != nil || off.State != OfflineVoid {
		t.Fatalf("LoadOffline = %+v, %v, want VOID", off, err)
	}
	key, err := UploadReport(t.Context(), obj, "eval", BuildReport(ReportInput{Date: "2026-11-03", Offline: off}))
	if err != nil || key != "report/daily/20261103.html" {
		t.Fatalf("UploadReport = %s, %v", key, err)
	}
	if !regexp.MustCompile(`实验 9 本期作废`).Match(obj.m["eval/"+key]) {
		t.Fatal("void banner missing")
	}
	// 指针存在、metrics.json 缺失：无数据；导入只取 Status=OK 的切片。
	ok, err := json.Marshal(model.LatestNightly{ExperimentID: 10})
	if err != nil {
		t.Fatal(err)
	}
	obj.m["eval/"+model.LatestNightlyKey] = ok
	if off, err := LoadOffline(t.Context(), obj, "eval"); err != nil || off.State != OfflineNoData {
		t.Fatalf("LoadOffline(missing metrics) = %+v, %v", off, err)
	}
	und := offMetric(model.MetricFactConsistency, model.SliceAll, 0, 0)
	und.Status = model.MetricStatusUndeterminable
	rows := OfflineRows("2026-11-03", synthOffline(10, offMetric(model.MetricCitationCoverage, model.SliceAll, 0.9, 50), und))
	if len(rows) != 1 || rows[0].ExperimentID != 10 || rows[0].Source != model.MetricSourceOffline || rows[0].TenantID != 0 {
		t.Fatalf("OfflineRows = %+v", rows)
	}
}
