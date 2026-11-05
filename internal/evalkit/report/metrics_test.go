package report

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

func metricsTrial(id string, idx int, runID uint64, sc ...evalkit.Score) evalkit.Trial {
	return evalkit.Trial{SampleID: id, SampleVersion: 1, TrialIndex: idx, RunID: runID, Scores: sc}
}

func citeScore(pass bool) evalkit.Score {
	return evalkit.Score{Name: "citation_valid", RedLine: true, Pass: pass, Evidence: map[bool]string{false: "reasons[0] has no citation"}[pass]}
}

func metricsExperiment() *evalkit.Experiment {
	return &evalkit.Experiment{ID: "42", FinishedAt: time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC), Trials: []evalkit.Trial{
		metricsTrial("a", 0, 1001, citeScore(true)), metricsTrial("a", 1, 1002, citeScore(true)),
		metricsTrial("b", 0, 1003, citeScore(false)), metricsTrial("b", 1, 1004, citeScore(true)),
		metricsTrial("c", 0, 1005, citeScore(true)),
	}}
}

func findMetric(t *testing.T, om model.OfflineMetrics, metric, slice string) model.OfflineMetric {
	t.Helper()
	for _, m := range om.Metrics {
		if m.Metric == metric && m.Slice == slice {
			return m
		}
	}
	t.Fatalf("metric %s slice %s missing", metric, slice)
	return model.OfflineMetric{}
}

func TestBuildMetrics_SlicesAndDrilldown(t *testing.T) {
	attrs := map[string]SampleAttrs{"a": {Category: "protect", Difficulty: "easy"}, "b": {Category: "protect", Traps: []string{"trap.overdose"}}}
	om, err := BuildMetrics(MetricsInput{Exp: metricsExperiment(), Attrs: attrs, Seed: 7, Resamples: 100})
	if err != nil {
		t.Fatal(err)
	}
	if om.ExperimentID != 42 || om.Seed != 7 || om.Resamples != 100 {
		t.Fatalf("header = %+v", om)
	}
	all := findMetric(t, om, "citation_coverage", model.SliceAll)
	// 样本级：a=1、b=0（红线取最小）、c=1 → 2/3；trial 级 4/5。
	if all.Status != model.MetricStatusOK || all.Samples != 3 || all.Numerator != 4 || all.Denominator != 5 ||
		all.Interval != "wilson" || !all.RedLine || !all.Calibrated || len(all.Failing) != 1 || all.Failing[0] != "b@1" {
		t.Fatalf("all slice = %+v", all)
	}
	if p := findMetric(t, om, "citation_coverage", "category=protect"); p.Samples != 2 || p.Point != 0.5 {
		t.Fatalf("category slice = %+v", p)
	}
	if tr := findMetric(t, om, "citation_coverage", "trap=trap.overdose"); tr.Samples != 1 || tr.Point != 0 {
		t.Fatalf("trap slice = %+v", tr)
	}
	findMetric(t, om, "citation_coverage", "difficulty=easy")
	// judge 指标：未校准，无 judge 打分 → NO_DATA；引用正确率只出报告也在列。
	if fc := findMetric(t, om, "fact_consistency", model.SliceAll); fc.Calibrated || !fc.Judge || fc.Status != model.MetricStatusNoData {
		t.Fatalf("judge metric = %+v", fc)
	}
	findMetric(t, om, "citation_correctness", model.SliceAll)
	if len(om.Metrics)%7 != 0 {
		t.Fatalf("each slice must carry all 7 metrics: %d", len(om.Metrics))
	}

	// 下钻：Failing 的样本在下钻页有锚点，锚点下列出该样本全部 trial 的 run_id。
	html, err := RenderDrilldown(metricsExperiment())
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	anchor := `id="` + model.SampleAnchor(all.Failing[0]) + `"`
	i := strings.Index(page, anchor)
	if i < 0 {
		t.Fatalf("drilldown missing %s", anchor)
	}
	section := page[i:]
	section = section[:strings.Index(section, "</section>")]
	for _, run := range []string{"1003", "1004"} {
		if !strings.Contains(section, `id="run-`+run+`"`) {
			t.Errorf("sample b section missing run_id %s", run)
		}
	}
}

// memStore 在 memPutter 上加 GetObject（不存在返回 model.ErrNotFound），供 UploadMetrics 读 latest.json。
type memStore struct {
	*memPutter
	getErr error
}

func newMemStore() *memStore { return &memStore{memPutter: newMemPutter()} }

func (m *memStore) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func readLatest(t *testing.T, obj *memStore) model.LatestNightly {
	t.Helper()
	var latest model.LatestNightly
	if err := json.Unmarshal(obj.objects["b/"+LatestNightlyKey], &latest); err != nil {
		t.Fatalf("latest.json: %v", err)
	}
	return latest
}

// grader_error 存在时当次不出数：BuildMetrics 报错，UploadMetrics 不写 metrics.json，只把 nightly/latest.json
// 置为本期作废（void），日报据此显示「本期作废」而不是沿用上一期的数。
func TestUploadMetrics_GraderErrorNotWritten(t *testing.T) {
	exp := metricsExperiment()
	exp.Trials = append(exp.Trials, metricsTrial("d", 0, 1006, evalkit.Score{Name: "citation_valid", Error: "boom"}))
	if _, err := BuildMetrics(MetricsInput{Exp: exp, Seed: 1, Resamples: 10}); err == nil {
		t.Fatal("grader_error must not build metrics")
	}
	obj := newMemStore()
	up, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: exp, Seed: 1, Resamples: 10})
	if err != nil || up.Key != "" || up.VoidReason != VoidGraderError || !up.LatestMoved || len(obj.objects) != 1 {
		t.Fatalf("grader_error upload = %+v %v objects=%d", up, err, len(obj.objects))
	}
	if _, ok := obj.objects["b/42/metrics.json"]; ok {
		t.Fatal("grader_error must not write metrics.json")
	}
	if latest := readLatest(t, obj); latest.ExperimentID != 42 || !latest.Void || latest.VoidReason != VoidGraderError {
		t.Fatalf("latest = %+v", latest)
	}
}

// P2-L3 修复 P1-2：门禁终态非 DONE（VOID / BUDGET_EXCEEDED）时不写 metrics.json，latest.json 记 void 与原因。
func TestUploadMetrics_GateVoid(t *testing.T) {
	obj := newMemStore()
	up, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: metricsExperiment(), Seed: 1, Resamples: 10,
		VoidReason: "BUDGET_EXCEEDED"})
	if err != nil || up.Key != "" || up.VoidReason != "BUDGET_EXCEEDED" || len(obj.objects) != 1 {
		t.Fatalf("gate void upload = %+v %v objects=%v", up, err, obj.objects)
	}
	if latest := readLatest(t, obj); !latest.Void || latest.VoidReason != "BUDGET_EXCEEDED" {
		t.Fatalf("latest = %+v", latest)
	}
}

// P2-L3 修复 P1-2：latest.json 只前进。补跑旧实验（started_at 更早，finished_at 被 resume 改成更晚）不回指；
// 同一实验重写、更新的实验照常前移；读指针失败不盲写。
func TestUploadMetrics_LatestOnlyMovesForward(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 18, 0, 0, 0, time.UTC) }
	exp := func(id string, started, finished time.Time) *evalkit.Experiment {
		e := metricsExperiment()
		e.ID, e.StartedAt, e.FinishedAt = id, started, finished
		return e
	}
	upload := func(obj *memStore, e *evalkit.Experiment) MetricsUpload {
		t.Helper()
		up, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: e, Seed: 1, Resamples: 10})
		if err != nil {
			t.Fatal(err)
		}
		return up
	}
	obj := newMemStore()
	if up := upload(obj, exp("50", day(3), day(3))); !up.LatestMoved {
		t.Fatal("first nightly must set latest")
	}
	// 补跑 11-02 的实验 49：finished_at 是补跑时刻（晚于 50），仍不得覆盖。
	if up := upload(obj, exp("49", day(2), day(4))); up.LatestMoved || up.Key != "49/metrics.json" {
		t.Fatalf("resumed older exp = %+v, must write its metrics.json but not move latest", up)
	}
	if latest := readLatest(t, obj); latest.ExperimentID != 50 {
		t.Fatalf("latest = %+v, want 50", latest)
	}
	if up := upload(obj, exp("50", day(3), day(4))); !up.LatestMoved {
		t.Fatal("re-run of the latest exp must rewrite latest")
	}
	if up := upload(obj, exp("51", day(4), day(4))); !up.LatestMoved || readLatest(t, obj).ExperimentID != 51 {
		t.Fatalf("newer exp must move latest: %+v", up)
	}
	obj.getErr = errors.New("minio down")
	if _, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: exp("52", day(5), day(5)), Seed: 1, Resamples: 10}); err == nil {
		t.Fatal("latest read failure must surface")
	}
}

func TestUploadMetrics_WritesMetricsAndLatest(t *testing.T) {
	obj := newMemStore()
	up, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: metricsExperiment(), Seed: 1, Resamples: 10})
	if err != nil || up.Key != "42/metrics.json" || up.VoidReason != "" || !up.LatestMoved {
		t.Fatalf("upload = %+v %v", up, err)
	}
	var om model.OfflineMetrics
	if err := json.Unmarshal(obj.objects["b/42/metrics.json"], &om); err != nil || om.ExperimentID != 42 {
		t.Fatalf("metrics.json = %+v, %v", om, err)
	}
	if latest := readLatest(t, obj); latest.ExperimentID != 42 || latest.Void {
		t.Fatalf("latest = %+v", latest)
	}
	obj.failKey = LatestNightlyKey
	if _, err := UploadMetrics(context.Background(), obj, "b", MetricsInput{Exp: metricsExperiment(), Seed: 1, Resamples: 10}); err == nil {
		t.Fatal("latest upload failure must surface")
	}
}
