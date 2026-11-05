package backflow

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/piiclean"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// 构造数据（无线上数据，待线上数据）：真实 ID 故意取醒目值，便于断言草稿里一个都不出现。
const (
	tenantA  = 1
	farmA    = 7301
	farmB    = 7302
	fieldA   = 88123
	userA    = 55017
	runA     = 990011
	runB     = 990012 // farmB 的 run
	svcUser  = 9900
	phoneA   = "13812345678"
	idCardA  = "110101199003071234"
	ownerA   = "张三丰"
	latExact = "31.234567"
	lngExact = "121.456789"
)

// now 是周一上午（上一 ISO 周 = 2026W44）。
var now = time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)

var lastWeek = time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

// 原文是 mcp.Result JSON（data / source / data_freshness_sec / collected_at），夹带身份、手机号、身份证、精确坐标与真实 ID。
var (
	weatherRaw = []byte(`{"data":{"farm_id":7301,"field_id":88123,"wind_speed_ms":2.0,"rain_mm_24h":0,` +
		`"location":{"lat":31.234567,"lng":121.456789},"station":"站点 31.234567,121.456789"},` +
		`"source":"mock-station","data_freshness_sec":600,"collected_at":"2026-10-28T02:50:00Z"}`)
	profileRaw = []byte(`{"data":{"field_id":88123,"area_mu":40.0,"owner":"张三丰","phone":"13812345678",` +
		`"remark":"联系 138-1234-5678 或身份证 110101199003071234，一三九一二三四五六七八","note":"北纬31.234567 东经121.456789","user_id":55017,` +
		`"device_id":4402187,"crop":"rice"},"source":"mock-field","data_freshness_sec":0,"collected_at":"2026-10-28T02:55:00Z"}`)
)

type fixture struct {
	farms *fakeHits
	store *fakeStore
	raw   *fakeRaw
	locks *fakeLocks
	dir   string
	deps  JobDeps
	job   *Job
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		farms: &fakeHits{byFarm: map[uint64][]model.AttributionHit{}},
		store: newFakeStore(),
		raw:   &fakeRaw{objects: map[runKey]map[uint64][]byte{}, expired: map[uint64]bool{}},
		locks: &fakeLocks{},
		dir:   t.TempDir(),
	}
	f.store.addRun(tenantA, farmA, model.BackflowMaterial{RunID: runA, FieldID: fieldA, UserID: userA,
		Question: "我手机13800138000，3号田稻飞虱要不要打药？", AskedAt: lastWeek, KnowledgeRef: "kb-rice@v3", Crop: "rice",
		Tools: []model.BackflowTool{{Name: "weather.forecast", RawID: 501}, {Name: "field.profile", RawID: 502},
			{Name: "weather.forecast", RawID: 503}}})
	f.raw.objects[runKey{tenantA, farmA, runA}] = map[uint64][]byte{501: weatherRaw, 502: profileRaw, 503: weatherRaw}
	f.store.addRun(tenantA, farmB, model.BackflowMaterial{RunID: runB, FieldID: 1, UserID: 2, AskedAt: lastWeek})
	f.raw.objects[runKey{tenantA, farmB, runB}] = map[uint64][]byte{}
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f.deps = JobDeps{
		Farms: fakeFarms{{TenantID: tenantA, FarmID: farmA, Timezone: "Asia/Shanghai"}, {TenantID: tenantA, FarmID: farmB}},
		Hits:  f.farms, Store: f.store, Raw: f.raw, Fetch: f.raw, Locks: f.locks, Keys: keys,
		Clock: fixedClock{now}, Logger: quiet(), System: repository.NewSystemScope(JobCollect), Loc: time.UTC,
		DraftsDir: f.dir, KBMissMinHits: 3, MaxRuns: 50, ServiceUserID: svcUser, Rand: rand.NewChaCha8([32]byte{7}),
	}
	f.rebuild(t, func(*JobDeps) {})
	return f
}

// rebuild 按改过的依赖重建任务。
func (f *fixture) rebuild(t *testing.T, edit func(*JobDeps)) {
	t.Helper()
	edit(&f.deps)
	job, err := NewJob(f.deps)
	if err != nil {
		t.Fatal(err)
	}
	f.job = job
}

func hit(farm uint64, source, reason, qtype string, run uint64) model.AttributionHit {
	return model.AttributionHit{TenantID: tenantA, FarmID: farm, Reason: reason, QuestionType: qtype, RunID: run,
		Source: source, At: lastWeek}
}

func (f *fixture) draftFile(t *testing.T, farm, run uint64) []byte {
	t.Helper()
	ref := f.store.drafts[runKey{tenantA, farm, run}]
	if ref == "" {
		t.Fatalf("run %d has no draft_ref (status %q)", run, f.store.status[runKey{tenantA, farm, run}])
	}
	b, err := os.ReadFile(filepath.Join(f.dir, ref+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestCollect_DesensitizedDraft（脱敏红线）：草稿不含真实 farm / field / user / run ID、手机号、身份证、身份、精确坐标；
// 坐标粗化到 0.01°；逐行过 pii_clean；按 sample 严格解码（字段对齐 fixtures）；expect 留空待专家。
func TestCollect_DesensitizedDraft(t *testing.T) {
	f := newFixture(t)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	out, err := f.job.RunOnce(context.Background())
	if err != nil || !out.Ran || out.Selected != 1 || out.Drafted != 1 || out.PIIFailed != 0 {
		t.Fatalf("RunOnce = %+v, %v", out, err)
	}
	if got := f.store.status[runKey{tenantA, farmA, runA}]; got != model.BackflowDrafted {
		t.Fatalf("status = %q, want DRAFTED", got)
	}
	raw := f.draftFile(t, farmA, runA)
	text := string(raw)
	for _, leak := range []string{"7301", "88123", "55017", "990011", "4402187", phoneA, "1234-5678", "13800138000",
		idCardA, ownerA, latExact, lngExact, "31.2345", "121.4567", "run_id", "一三九一二三四五六七八"} {
		if strings.Contains(text, leak) {
			t.Errorf("draft contains %q:\n%s", leak, text)
		}
	}
	for _, want := range []string{"31.23", "121.46", "mock-station", "kb-rice@v3", "wind_speed_ms", "联系 syn-phone-",
		"身份证 syn-id-card-"} {
		if !strings.Contains(text, want) {
			t.Errorf("draft missing %q:\n%s", want, text)
		}
	}
	for i, line := range strings.Split(text, "\n") {
		if l, ok := piiclean.Detect(line); ok {
			t.Errorf("line %d pii_clean hit %s %s: %s", i+1, l.Kind, l.Value, line)
		}
	}
	s, err := sample.Parse(raw)
	if err != nil {
		t.Fatalf("draft not a sample: %v\n%s", err, text)
	}
	ref := f.store.drafts[runKey{tenantA, farmA, runA}]
	if s.ID != ref || s.Suite != "capability" || s.ReviewStatus != "pending_expert" || s.KnowledgeRef != "kb-rice@v3" ||
		s.Crop != "rice" || s.Expect.Outcome != "" || s.Fixtures.Clock != "2026-10-28T11:00:00+08:00" {
		t.Fatalf("sample = %+v", s)
	}
	if !strings.HasPrefix(s.Context.Farm, "farm-bf-") || !strings.HasPrefix(s.Context.Field, "field-bf-") ||
		!strings.HasPrefix(s.Context.User, "u-bf-") {
		t.Fatalf("context = %+v, want synthetic codes", s.Context)
	}
	weather := s.Fixtures.MCP["weather.forecast"]
	if len(weather) != 1 || len(weather[0].Returns) != 2 || weather[0].Match["field"] != s.Context.Field {
		t.Fatalf("weather fixtures = %+v", weather)
	}
	profile := s.Fixtures.MCP["field.profile"]
	if len(profile) != 1 || profile[0].Return == nil {
		t.Fatalf("profile fixtures = %+v", profile)
	}
	pm, ok := profile[0].Return.(map[string]any)
	if !ok || pm["field_id"] != s.Context.Field || pm["source"] != "mock-field" || fmt.Sprint(pm["area_mu"]) != "40" {
		t.Fatalf("profile return = %#v (field_id must be the context field code)", profile[0].Return)
	}
	if strings.Contains(s.Question, "13800138000") || !strings.Contains(s.Question, "稻飞虱") {
		t.Fatalf("question = %q", s.Question)
	}
}

// TestCollect_RawReadAudited：每条有原文的工具调用恰好一次 ReadRaw（= 一条 RAW_READ 审计），以服务账号（auditor）、
// 本农场 Scope、本 run 读；重跑不再读（状态已推进）。
func TestCollect_RawReadAudited(t *testing.T) {
	f := newFixture(t)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceExpert, model.AttrReasonExpertWrong, "pest", runA)}
	if _, err := f.job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.raw.reads) != 3 {
		t.Fatalf("RAW_READ count = %d, want 3", len(f.raw.reads))
	}
	for i, r := range f.raw.reads {
		if r.scope.TenantID != tenantA || r.scope.FarmID != farmA || r.scope.UserID != svcUser || r.runID != runA ||
			len(r.scope.Roles) != 1 || r.scope.Roles[0] != model.RoleAuditor {
			t.Errorf("read %d = %+v, want service auditor of farm %d run %d", i, r, farmA, runA)
		}
	}
	if got := f.locks.keys; len(got) != 1 || got[0] != "as:test:lock:p2job:backflow-collect" {
		t.Fatalf("lock keys = %v", got)
	}
	out, err := f.job.RunOnce(context.Background())
	if err != nil || out.Selected != 0 || out.Drafted != 0 || len(f.raw.reads) != 3 {
		t.Fatalf("rerun = %+v, %v, reads %d; want idempotent", out, err, len(f.raw.reads))
	}
	if files, _ := os.ReadDir(f.dir); len(files) != 1 {
		t.Fatalf("draft files = %d, want 1", len(files))
	}
}

// TestCollect_CrossFarmIsolation（隔离红线）：仓储返回了他场的信号也不交给本场 Scope 选样，不读他场原文。
func TestCollect_CrossFarmIsolation(t *testing.T) {
	f := newFixture(t)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmB, model.EvidenceFeedback, "TIMING", "pest", runB),
		{TenantID: tenantA + 1, FarmID: farmA, Source: model.EvidenceFeedback, RunID: runA, At: lastWeek}}
	out, err := f.job.RunOnce(context.Background())
	if err != nil || out.Selected != 0 {
		t.Fatalf("RunOnce = %+v, %v; want nothing selected", out, err)
	}
	for _, c := range f.store.markCalls {
		if len(c.ids) > 0 {
			t.Fatalf("MarkSelected(%+v, %v): foreign run handed to scope", c.scope, c.ids)
		}
	}
	if len(f.raw.reads) != 0 || f.store.status[runKey{tenantA, farmB, runB}] != "" {
		t.Fatalf("reads = %+v, farmB status = %q", f.raw.reads, f.store.status[runKey{tenantA, farmB, runB}])
	}
}

// TestCollect_PIIFailed：脱敏后仍有 pii_clean 命中（手机号做了 JSON 键，脱敏不改键）→ PII_FAILED，不写草稿、不写 draft_ref。
func TestCollect_PIIFailed(t *testing.T) {
	f := newFixture(t)
	f.raw.objects[runKey{tenantA, farmA, runA}][502] = []byte(`{"data":{"13912345678":"值班"},"source":"mock-field"}`)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceValidation, model.AttrReasonGuardrail, "pest", runA)}
	out, err := f.job.RunOnce(context.Background())
	if err != nil || out.PIIFailed != 1 || out.Drafted != 0 {
		t.Fatalf("RunOnce = %+v, %v", out, err)
	}
	k := runKey{tenantA, farmA, runA}
	if f.store.status[k] != model.BackflowPIIFailed || f.store.drafts[k] != "" {
		t.Fatalf("status = %q ref = %q, want PII_FAILED without ref", f.store.status[k], f.store.drafts[k])
	}
	if files, _ := os.ReadDir(f.dir); len(files) != 0 {
		t.Fatalf("draft files = %d, want 0", len(files))
	}
}

// TestCollect_RealIDResidue：真实田块 ID 藏在非 ID 键下（脱敏认不出）→ 兜底扫描拦下，PII_FAILED。
func TestCollect_RealIDResidue(t *testing.T) {
	f := newFixture(t)
	f.raw.objects[runKey{tenantA, farmA, runA}][502] = []byte(`{"data":{"plot":"地块88123"},"source":"mock-field"}`)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	if out, err := f.job.RunOnce(context.Background()); err != nil || out.PIIFailed != 1 {
		t.Fatalf("RunOnce = %+v, %v; want PII_FAILED", out, err)
	}
}

// TestCollect_Selection：差评 / 专家判错 / 护栏直接入选；KB_MISS 同问题类型一周 ≥ 阈值才入选；驳回不入回流；MaxRuns 封顶。
func TestCollect_Selection(t *testing.T) {
	hits := []model.AttributionHit{
		hit(farmA, model.EvidenceRejection, "DOSAGE", "pest", 10),
		hit(farmA, model.EvidenceFeedback, "TIMING", "pest", 11),
		hit(farmA, model.EvidenceExpert, model.AttrReasonExpertWrong, "pest", 12),
		hit(farmA, model.EvidenceValidation, model.AttrReasonGuardrail, "pest", 13),
		hit(farmA, model.EvidenceKBMiss, model.AttrReasonKBMiss, "pest", 20),
		hit(farmA, model.EvidenceKBMiss, model.AttrReasonKBMiss, "pest", 21),
		hit(farmA, model.EvidenceKBMiss, model.AttrReasonKBMiss, "irrigation", 30),
		hit(farmA, model.EvidenceKBMiss, model.AttrReasonKBMiss, "irrigation", 31),
		hit(farmA, model.EvidenceKBMiss, model.AttrReasonKBMiss, "irrigation", 32),
		hit(farmA, model.EvidenceFeedback, "TIMING", "pest", 11), // 同 run 两个信号只选一次
	}
	scope := model.Scope{TenantID: tenantA, FarmID: farmA}
	got := pick(scope, hits, 3, 50)
	want := []uint64{11, 12, 13, 30, 31, 32}
	if len(got) != len(want) {
		t.Fatalf("pick = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pick = %v, want %v", got, want)
		}
	}
	if capped := pick(scope, hits, 3, 2); len(capped) != 2 || capped[0] != 11 || capped[1] != 12 {
		t.Fatalf("capped pick = %v", capped)
	}
}

// TestCollect_LockHeld：锁被持有时不选样、不读原文。
func TestCollect_LockHeld(t *testing.T) {
	f := newFixture(t)
	f.locks.held = true
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	out, err := f.job.RunOnce(context.Background())
	if err != nil || out.Ran || len(f.raw.reads) != 0 || len(f.farms.calls) != 0 {
		t.Fatalf("RunOnce = %+v, %v; reads %d", out, err, len(f.raw.reads))
	}
}

// TestCollect_ExpiredRaw：原文已过保留期的工具调用跳过（草稿 notes 注明），其余照常出草稿；其他读原文错误保留 SELECTED 待重试。
func TestCollect_ExpiredRaw(t *testing.T) {
	f := newFixture(t)
	f.raw.expired[503] = true
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	if out, err := f.job.RunOnce(context.Background()); err != nil || out.Drafted != 1 {
		t.Fatalf("RunOnce = %+v, %v", out, err)
	}
	s, err := sample.Parse(f.draftFile(t, farmA, runA))
	if err != nil {
		t.Fatal(err)
	}
	if w := s.Fixtures.MCP["weather.forecast"]; len(w) != 1 || w[0].Return == nil || !strings.Contains(s.Notes, "过保留期") {
		t.Fatalf("weather = %+v notes = %q", w, s.Notes)
	}

	g := newFixture(t)
	delete(g.raw.objects[runKey{tenantA, farmA, runA}], 502) // ReadRaw → ErrForbidden
	g.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	out, err := g.job.RunOnce(context.Background())
	if err != nil || out.Drafted != 0 || out.Retrying != 1 || g.store.status[runKey{tenantA, farmA, runA}] != model.BackflowSelected {
		t.Fatalf("forbidden raw = %+v, %v; want run kept SELECTED for retry, no job error", out, err)
	}
	if files, _ := os.ReadDir(g.dir); len(files) != 0 {
		t.Fatalf("draft files = %d, want 0", len(files))
	}
}

// TestCollect_FailedRunFreesSlot：读原文持续失败的 run 最多试 BackflowMaxAttempts 次（每次一组 RAW_READ）后置 FAILED，
// 不再占 SelectedRuns 名额，后来选中的 run 照常出草稿；单个 run 失败不让任务报错（上一周照常记为已跑）。
func TestCollect_FailedRunFreesSlot(t *testing.T) {
	const runC = 990013
	f := newFixture(t)
	f.rebuild(t, func(d *JobDeps) { d.MaxRuns = 1 })
	delete(f.raw.objects[runKey{tenantA, farmA, runA}], 502) // runA 第二条原文读不到 → 每次都失败
	f.store.addRun(tenantA, farmA, model.BackflowMaterial{RunID: runC, FieldID: fieldA, UserID: userA, AskedAt: lastWeek,
		Question: "要不要灌水？", Tools: []model.BackflowTool{{Name: "weather.forecast", RawID: 601}}})
	f.raw.objects[runKey{tenantA, farmA, runC}] = map[uint64][]byte{601: weatherRaw}
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	ctx := context.Background()
	kA, kC := runKey{tenantA, farmA, runA}, runKey{tenantA, farmA, runC}
	for attempt := 1; attempt <= model.BackflowMaxAttempts; attempt++ {
		out, err := f.job.RunOnce(ctx)
		if err != nil {
			t.Fatalf("attempt %d: RunOnce err = %v; a single run failure must not fail the job", attempt, err)
		}
		if attempt == 1 {
			// 第二周起 runC 也入选，但 runA 在重试期间占着唯一名额。
			f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "irrigation", runC)}
		}
		want := model.BackflowSelected
		if attempt == model.BackflowMaxAttempts {
			want = model.BackflowFailed
		}
		if f.store.status[kA] != want || f.store.status[kC] == model.BackflowDrafted {
			t.Fatalf("attempt %d: runA %q runC %q, out %+v", attempt, f.store.status[kA], f.store.status[kC], out)
		}
	}
	reads := len(f.raw.reads)
	out, err := f.job.RunOnce(ctx)
	if err != nil || out.Drafted != 1 || f.store.status[kC] != model.BackflowDrafted || f.store.status[kA] != model.BackflowFailed {
		t.Fatalf("after FAILED: %+v, %v; runA %q runC %q", out, err, f.store.status[kA], f.store.status[kC])
	}
	for _, r := range f.raw.reads[reads:] {
		if r.runID == runA {
			t.Fatalf("FAILED run %d read again: %+v", runA, r)
		}
	}
}

// TestCollect_TickAdvancesOnRunFailure：有 run 失败时 Tick 仍把上一周记为已跑，同周再 Tick 不重读原文。
func TestCollect_TickAdvancesOnRunFailure(t *testing.T) {
	f := newFixture(t)
	delete(f.raw.objects[runKey{tenantA, farmA, runA}], 502)
	f.farms.byFarm[farmA] = []model.AttributionHit{hit(farmA, model.EvidenceFeedback, "TIMING", "pest", runA)}
	if err := f.job.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	reads := len(f.raw.reads)
	if err := f.job.Tick(context.Background()); err != nil || len(f.raw.reads) != reads {
		t.Fatalf("second tick: err %v, reads %d → %d; want no rerun in the same week", err, reads, len(f.raw.reads))
	}
}

// TestNewJob_Deps：依赖缺失报 ErrJobDeps。
func TestNewJob_Deps(t *testing.T) {
	if _, err := NewJob(JobDeps{}); !errors.Is(err, ErrJobDeps) {
		t.Fatalf("err = %v, want ErrJobDeps", err)
	}
}
