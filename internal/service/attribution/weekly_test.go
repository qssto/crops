package attribution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

var cst = time.FixedZone("CST", 8*3600)

func TestPrevWeek(t *testing.T) {
	// 2026-11-09 是周一：CST 00:30 时上一周为 2026W45 [11-02, 11-09)
	w := PrevWeek(time.Date(2026, 11, 8, 16, 30, 0, 0, time.UTC), cst)
	if w.Name != "2026W45" || !w.From.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, cst)) || !w.To.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, cst)) {
		t.Fatalf("PrevWeek = %+v", w)
	}
	// UTC 仍是周日 23:00，但 CST 已是周一：按配置时区切
	if got := PrevWeek(time.Date(2026, 11, 8, 23, 0, 0, 0, time.UTC), time.UTC).Name; got != "2026W44" {
		t.Fatalf("PrevWeek UTC = %s, want 2026W44", got)
	}
}

type harness struct {
	clk    *stepClock
	hits   *fakeHits
	issues *fakeIssues
	obj    *memObjects
	locks  *fakeLocks
	job    *Job
}

func newHarness(t *testing.T, start time.Time) *harness {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{clk: &stepClock{start}, hits: &fakeHits{flow: map[uint64]map[string]int{}}, issues: &fakeIssues{},
		obj: &memObjects{}, locks: &fakeLocks{}}
	h.job, err = NewJob(JobDeps{Farms: fakeFarms{{TenantID: 1, FarmID: 1, Timezone: "Asia/Shanghai"},
		{TenantID: 1, FarmID: 2, Timezone: "Asia/Shanghai"}}, Hits: h.hits, Issues: h.issues, Objects: h.obj, Bucket: "eval",
		Locks: h.locks, Keys: keys, Clock: h.clk, Logger: quiet(), System: repository.NewSystemScope(JobWeekly), Loc: cst,
		TopN: 2, KBLoadGapsKey: "nightly/kb_load_gaps.json"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// hit 造一条 farm 的信号（At 为周界时区下的时刻）。
func (h *harness) hit(farm uint64, reason, kb string, id uint64, at time.Time) {
	h.hits.hits = append(h.hits.hits, model.AttributionHit{TenantID: 1, FarmID: farm, Reason: reason, QuestionType: "pest",
		PrimaryKB: kb, RunID: id, Source: model.EvidenceRejection, SourceID: id, At: at})
}

func (h *harness) report(t *testing.T, week string) string {
	t.Helper()
	b, ok := h.obj.m["eval/"+ReportKey(week)]
	if !ok {
		t.Fatalf("report %s missing; objects %v", week, keysOf(h.obj.m))
	}
	return string(b)
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 应失败：非周一也出周报以外的重复出报、跨农场聚合漏农场、RESOLVED 再命中不重开、上期处置不回填、kb_load_gaps 缺失时报错。
// 注入时钟连跑 4 周（2026W45-W48），每周一 CST 00:10 触发。
func TestJob_FourWeeks(t *testing.T) {
	mon := time.Date(2026, 11, 9, 0, 10, 0, 0, cst) // 2026W46 周一
	h := newHarness(t, mon)
	ctx := context.Background()
	w45 := time.Date(2026, 11, 3, 10, 0, 0, 0, cst)
	sigA := model.Signature("DOSAGE", "pest", "kb:a")
	// W45：A 在两个农场共 3 次，B 1 次；窗口外（W46）的一条不计
	h.hit(1, "DOSAGE", "kb:a", 1, w45)
	h.hit(1, "DOSAGE", "kb:a", 2, w45.Add(time.Hour))
	h.hit(2, "DOSAGE", "kb:a", 3, w45)
	h.hit(2, "TIMING", "kb:b", 4, w45)
	h.hit(1, "DOSAGE", "kb:a", 5, mon.Add(time.Hour))
	h.hits.flow[1] = map[string]int{model.BackflowSelected: 3, model.BackflowDrafted: 2, model.BackflowMerged: 1}

	if err := h.job.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	a := h.issues.rows[sigA]
	if a == nil || a.HitCount != 3 || a.FirstWeek != "2026W45" || len(a.Evidence) != 3 || a.Evidence[0].RunID != 2 {
		t.Fatalf("W45 issue A = %+v", a)
	}
	if want := []uint64{1, 2}; fmt.Sprint(h.hits.calls) != fmt.Sprint(want) {
		t.Fatalf("farms queried = %v, want %v", h.hits.calls, want)
	}
	rep := h.report(t, "2026W45")
	for _, w := range []string{"归因周报 2026W45", "kb:a", "无上期周报", "本周无重开", "本周选入回流 3 个 run，入库 1 个",
		"离线知识加载缺口", "待调度方决定 server 连评测库"} {
		if !strings.Contains(rep, w) {
			t.Errorf("W45 report missing %q", w)
		}
	}
	if !strings.Contains(rep[strings.Index(rep, "离线知识加载缺口"):], "无") {
		t.Errorf("gaps section should say 无 when kb_load_gaps.json missing")
	}
	if got := h.locks.keys; len(got) != 1 || got[0] != "as:test:lock:p2job:attribution-weekly" {
		t.Fatalf("lock keys = %v", got)
	}

	// 同一周再 Tick（重启 / 另一副本）：done 标记存在即跳过
	h.clk.t = mon.Add(3 * time.Hour)
	if err := h.job.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.locks.keys) != 1 {
		t.Fatalf("second tick in same week re-ran: lock keys %v", h.locks.keys)
	}

	// 运营处置 A：RESOLVED（模拟 PATCH）
	disp, ref := model.DispositionAddKB, "kb PR #1"
	a.Status, a.Disposition, a.ResolutionRef = model.IssueResolved, &disp, &ref

	// W46：A 再命中 → 重开；上期处置回填显示 A 的处置
	h.clk.t = mon.AddDate(0, 0, 7)
	h.hits.hits = h.hits.hits[:0]
	h.hit(1, "DOSAGE", "kb:a", 6, mon.Add(time.Hour))
	if err := h.job.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Status != model.IssueOpen || a.ReopenCount != 1 || a.HitCount != 1 || a.LastWeek != "2026W46" || len(a.Evidence) != 4 {
		t.Fatalf("W46 reopen A = %+v", a)
	}
	rep = h.report(t, "2026W46")
	reopenSec := rep[strings.Index(rep, "<h2>重开问题</h2>"):strings.Index(rep, "<h2>线上回流</h2>")]
	if !strings.Contains(reopenSec, "kb:a") || !strings.Contains(reopenSec, "kb PR #1") {
		t.Errorf("W46 reopened section = %s", reopenSec)
	}
	prevSec := rep[strings.Index(rep, "上期处置回填（2026W45）"):strings.Index(rep, "<h2>重开问题</h2>")]
	if !strings.Contains(prevSec, model.DispositionAddKB) || strings.Contains(prevSec, "无上期周报") {
		t.Errorf("W46 backfill section = %s", prevSec)
	}
	var snap Snapshot
	if err := json.Unmarshal(h.obj.m["eval/"+DoneKey("2026W46")], &snap); err != nil || len(snap.Reopened) != 1 || snap.Reopened[0] != sigA {
		t.Fatalf("W46 snapshot = %+v, %v", snap, err)
	}

	// 补跑 W46（done 标记写失败后下周期重出）：不再计重开，但报告仍列出本周重开
	delete(h.obj.m, "eval/"+DoneKey("2026W46"))
	if _, err := h.job.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if a.ReopenCount != 1 || !strings.Contains(h.report(t, "2026W46"), "kb PR #1") {
		t.Fatalf("rerun W46: reopen_count = %d", a.ReopenCount)
	}

	// W47：无信号
	h.clk.t = mon.AddDate(0, 0, 14)
	h.hits.hits = h.hits.hits[:0]
	if err := h.job.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.report(t, "2026W47"), "本周无负向信号") {
		t.Errorf("W47 report should show no signals")
	}

	// W48：L5 kb_load_gaps.json 存在 → 附一节
	h.obj.m["eval/nightly/kb_load_gaps.json"] = []byte(`{"kb_miss":[{"sample_id":"s-1","question_type":"pest","topic":"rice/pest","trials":2}],
"low_hit":[{"topic":"rice/disease","hit":{"Num":1,"Den":4},"hit_rate":0.25,"missing":["kb:rice/disease/blast"],"samples":["s-2"],"strategy":["v1"]}]}`)
	h.clk.t = mon.AddDate(0, 0, 21)
	if err := h.job.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	rep = h.report(t, "2026W48")
	for _, w := range []string{"KB_MISS", "rice/pest", "0.25", "kb:rice/disease/blast"} {
		if !strings.Contains(rep, w) {
			t.Errorf("W48 gaps section missing %q", w)
		}
	}
	if n := len(h.issues.rows); n != 2 {
		t.Fatalf("issue rows after 4 weeks = %d, want 2 (one per signature)", n)
	}
}

// TestJob_DoneAfterLockSkips：Tick 时尚无 done 标记，等锁期间另一副本已出完周报 → 取锁后复查到 done 即返回，
// 不重复 upsert、不覆盖周报。
func TestJob_DoneAfterLockSkips(t *testing.T) {
	h := newHarness(t, time.Date(2026, 11, 9, 0, 10, 0, 0, cst))
	h.hit(1, "DOSAGE", "kb:a", 1, time.Date(2026, 11, 3, 10, 0, 0, 0, cst))
	done := []byte(`{"week":"2026W45","report_key":"report/weekly/2026W45.html","top":[],"reopened":[]}`)
	h.locks.onAcquire = func() {
		if err := h.obj.PutObject(context.Background(), "eval", DoneKey("2026W45"), done, "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.job.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	out, err := h.job.RunOnce(context.Background())
	if err != nil || out.Ran || !out.Done {
		t.Fatalf("RunOnce with done marker: out=%+v err=%v, want Ran=false Done=true", out, err)
	}
	if len(h.issues.rows) != 0 || len(h.hits.calls) != 0 {
		t.Fatalf("re-ran after done: issues=%d hit calls=%v", len(h.issues.rows), h.hits.calls)
	}
	if _, ok := h.obj.m["eval/"+ReportKey("2026W45")]; ok {
		t.Fatal("report overwritten after done")
	}
}

func TestJob_LockHeldSkips(t *testing.T) {
	h := newHarness(t, time.Date(2026, 11, 9, 0, 10, 0, 0, cst))
	h.locks.held = map[string]bool{"as:test:lock:p2job:attribution-weekly": true}
	out, err := h.job.RunOnce(context.Background())
	if err != nil || out.Ran || len(h.obj.m) != 0 {
		t.Fatalf("lock held: out=%+v err=%v objects=%v", out, err, keysOf(h.obj.m))
	}
}

// 应失败：跨周累积证据超过 50 条。
func TestJob_EvidenceCappedAcrossWeeks(t *testing.T) {
	mon := time.Date(2026, 11, 9, 0, 10, 0, 0, cst)
	h := newHarness(t, mon)
	for week := range 2 {
		h.clk.t = mon.AddDate(0, 0, 7*week)
		h.hits.hits = h.hits.hits[:0]
		for i := range 30 {
			h.hit(1, "DOSAGE", "kb:a", uint64(week*100+i), h.clk.t.AddDate(0, 0, -3))
		}
		if err := h.job.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	a := h.issues.rows[model.Signature("DOSAGE", "pest", "kb:a")]
	if len(a.Evidence) != model.MaxEvidenceRefs || a.Evidence[0].Week != "2026W46" || a.Evidence[49].Week != "2026W45" {
		t.Fatalf("evidence len %d first %+v last %+v", len(a.Evidence), a.Evidence[0], a.Evidence[len(a.Evidence)-1])
	}
}

// 周报 HTML 转义：kb_id / 处置记录来自库，不得注入脚本。
func TestRenderReport_Escapes(t *testing.T) {
	kb, ref := "kb:<script>x</script>", `"><img src=x>`
	body, err := RenderReport(Report{Week: "2026W45", Top: []model.AttributionIssue{{ID: 1, PrimaryKB: &kb, ResolutionRef: &ref}},
		Reopened: []model.AttributionIssue{{ID: 1, PrimaryKB: &kb, ResolutionRef: &ref}}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(body); strings.Contains(s, "<script>x") || strings.Contains(s, "<img src=x>") {
		t.Fatalf("unescaped: %s", s)
	}
}

func TestNewJob_RequiresDeps(t *testing.T) {
	if _, err := NewJob(JobDeps{}); err == nil {
		t.Fatal("NewJob with zero deps should fail")
	}
}
