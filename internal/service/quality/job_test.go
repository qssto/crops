package quality

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service/quality/judge"
)

var (
	farmA = model.Scope{TenantID: 1, FarmID: 1}
	farmB = model.Scope{TenantID: 2, FarmID: 7}
	// now 是 2026-11-04 09:00 上海：前一日为 2026-11-03，UTC 窗口 [11-02 16:00, 11-03 16:00)。
	now   = time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	inDay = time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
)

const testVersion = "abcdef0123456789-standard"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// synthDay 是合成的一日数据：A 场 6 张出卡（5 张全引用）、2 张 VALIDATION_FAILED、2 张其他拒答；
// B 场 2 张出卡（都引用）、1 张拒答；另有一张窗口外的卡（前一日 15:59:59 UTC 之前，属于 11-02）。
func synthDay() *fakeStore {
	s := newFakeStore()
	s.farms = []repository.FarmTZ{{TenantID: 1, FarmID: 1, Timezone: "Asia/Shanghai"}, {TenantID: 2, FarmID: 7, Timezone: "Asia/Shanghai"}}
	id := uint64(0)
	add := func(scope model.Scope, at time.Time, cat string, o model.Outcome, reason string, cited bool) {
		id++
		s.cards = append(s.cards, fakeCard{scope: scope, id: id, run: 100 + id, at: at, category: cat, outcome: o, reason: reason, cited: cited})
	}
	for i := range 6 {
		cat := "protect"
		if i%2 == 1 {
			cat = "fertilize"
		}
		add(farmA, inDay, cat, model.OutcomeAdvice, "", i != 0)
	}
	add(farmA, inDay, "protect", model.OutcomeRefused, RefuseValidation, false)
	add(farmA, inDay, "protect", model.OutcomeRefused, RefuseValidation, false)
	add(farmA, inDay, "protect", model.OutcomeRefused, "OUT_OF_SCOPE", false)
	add(farmA, inDay, "protect", model.OutcomeRefused, "OUT_OF_SCOPE", false)
	add(farmB, inDay, "protect", model.OutcomeAdvice, "", true)
	add(farmB, inDay, "protect", model.OutcomeAdvice, "", true)
	add(farmB, inDay, "protect", model.OutcomeRefused, "OUT_OF_SCOPE", false)
	add(farmA, time.Date(2026, 11, 2, 15, 59, 59, 0, time.UTC), "protect", model.OutcomeAdvice, "", false)
	return s
}

func newTestJob(t *testing.T, s *fakeStore, j *fakeJudge, obj *memObjects, locks *fakeLocks, tokens *fakeTokens) (*Job, *RecordingNotifier) {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	n := &RecordingNotifier{}
	var sampler *Sampler
	if j != nil {
		sampler = &Sampler{Store: s, Cards: s, Judge: j, Version: testVersion, Tokens: tokens, ModelKey: keys.Raw("rl:model:standard"),
			JudgeKey: keys.RlJudge("standard"), JudgeQPM: 6, Quota: 2, Salt: "p2l3", Clock: fixedClock{now}, Logger: quiet(),
			Wait: func(context.Context, time.Duration) error { return nil }}
	}
	job, err := NewJob(JobDeps{Farms: s, Rollup: Rollup{Store: s, Version: testVersion}, Sampler: sampler, Reports: s,
		Objects: obj, Bucket: "eval", Notifier: n, Locks: locks, Keys: keys, Clock: fixedClock{now}, Logger: quiet(),
		ReportLoc: loc, BaselineName: "baseline_p1.r2"})
	if err != nil {
		t.Fatal(err)
	}
	return job, n
}

func platform(s *fakeStore, metric string) (model.QualityDaily, bool) {
	for _, q := range s.daily {
		if q.TenantID == 0 && q.FarmID == 0 && q.Source == model.MetricSourceOnline && q.Metric == metric && q.StatDate == "2026-11-03" {
			return q, true
		}
	}
	return model.QualityDaily{}, false
}

func TestJob_RollupFarmAndPlatformRows(t *testing.T) {
	s := synthDay()
	obj := &memObjects{}
	tokens := &fakeTokens{}
	locks := &fakeLocks{}
	job, n := newTestJob(t, s, &fakeJudge{}, obj, locks, tokens)
	out, err := job.RunOnce(t.Context())
	if err != nil || !out.Ran {
		t.Fatalf("RunOnce = %+v, %v", out, err)
	}
	if out.Date != "2026-11-03" || out.ReportKey != "report/daily/20261103.html" {
		t.Fatalf("date / key = %s %s", out.Date, out.ReportKey)
	}
	a := out.Farms[keyOf(farmA)].Counts
	if a != (AdviceCounts{Total: 10, Advice: 6, Cited: 5, Refused: 4, ValidationFailed: 2}) {
		t.Fatalf("farm A counts = %+v (window must exclude the 11-02 card)", a)
	}
	// 平台行 = 两场农场行之和：引用覆盖 (5+2)/(6+2)，数值准确 (6+2)/(8+2)，拒答率 (4+1)/(10+3)。
	want := map[string][2]int{model.MetricCitationCoverage: {7, 8}, model.MetricNumericAccuracy: {8, 10}, model.MetricRefusalRate: {5, 13}}
	for m, w := range want {
		q, ok := platform(s, m)
		if !ok || q.Numerator != w[0] || q.Denominator != w[1] {
			t.Fatalf("platform %s = %+v, want %v", m, q, w)
		}
	}
	// 抽样：A 两类各 2 张、B 一类 2 张 = 6 张，judge 全 3 分。
	fc, ok := platform(s, model.MetricFactConsistency)
	if !ok || fc.Numerator != 6 || fc.Denominator != 6 || !fc.IsEstimate {
		t.Fatalf("platform fact_consistency = %+v, want 6/6 estimate", fc)
	}
	// 令牌顺序：每张卡先 rl:model 再 rl:judge。
	if len(tokens.keys) != 12 || !strings.HasSuffix(tokens.keys[0], "rl:model:standard") || !strings.HasSuffix(tokens.keys[1], "rl:judge:standard") {
		t.Fatalf("token keys = %v", tokens.keys)
	}
	if !slices.Equal(locks.keys, []string{"as:test:lock:p2job:online-judge-sample", "as:test:lock:p2job:quality-metric-rollup"}) {
		t.Fatalf("lock keys = %v", locks.keys)
	}
	if locks.renews == 0 {
		t.Fatal("job lock never renewed")
	}
	if _, ok := obj.m["eval/report/daily/20261103.html"]; !ok {
		t.Fatalf("daily report not uploaded: %v", obj.m)
	}
	if sent := n.Sent(); len(sent) != 1 || !strings.Contains(sent[0], "可信度日报 2026-11-03") {
		t.Fatalf("notify = %v", sent)
	}
}

// 补跑：同一日再跑一次，judge 不重复调用，日指标同键覆盖、取值不变。
func TestJob_RerunIdempotent(t *testing.T) {
	s := synthDay()
	j := &fakeJudge{}
	job, _ := newTestJob(t, s, j, &memObjects{}, &fakeLocks{}, &fakeTokens{})
	if _, err := job.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := len(s.daily)
	snapshot := map[string]model.QualityDaily{}
	for k, v := range s.daily {
		snapshot[k] = v
	}
	calls := j.calls
	if _, err := job.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if j.calls != calls || len(s.results) != 6 {
		t.Fatalf("rerun judged again: calls %d -> %d, results %d", calls, j.calls, len(s.results))
	}
	if len(s.daily) != before {
		t.Fatalf("rerun rows %d -> %d", before, len(s.daily))
	}
	for k, v := range snapshot {
		if g := s.daily[k]; g.Numerator != v.Numerator || g.Denominator != v.Denominator || g.Point != v.Point {
			t.Fatalf("rerun changed %s: %+v -> %+v", k, v, g)
		}
	}
}

// P2-L3 修复 P1-3：一个农场时区非法只跳过该农场，其余农场照常汇总、出日报并推送，整轮不报错。
func TestJob_BadTimezoneSkipsFarmOnly(t *testing.T) {
	s := synthDay()
	s.farms = append(s.farms, repository.FarmTZ{TenantID: 3, FarmID: 9, Timezone: "Mars/Olympus"})
	obj := &memObjects{}
	job, n := newTestJob(t, s, &fakeJudge{}, obj, &fakeLocks{}, &fakeTokens{})
	if err := job.Tick(t.Context()); err != nil {
		t.Fatalf("Tick with one bad timezone = %v, want nil", err)
	}
	if q, ok := platform(s, model.MetricCitationCoverage); !ok || q.Numerator != 7 || q.Denominator != 8 {
		t.Fatalf("platform citation_coverage = %+v, other farms must still roll up", q)
	}
	if len(n.Sent()) != 1 {
		t.Fatalf("notify = %v, want 1", n.Sent())
	}
	if _, ok := obj.m["eval/"+model.DailyNotifiedKey("2026-11-03")]; !ok {
		t.Fatal("notified marker not written: the day must count as done")
	}
}

// P2-L3 修复 P1-3：「已完成」持久在桶里的推送标记。另一实例 / 重启后的 Tick 看到标记即跳过；手动 RunOnce 补跑
// 重写日报但不再推送；读标记失败本周期不跑。
func TestJob_NotifiedMarkerPreventsRepeat(t *testing.T) {
	s := synthDay()
	obj := &memObjects{}
	j := &fakeJudge{}
	first, n1 := newTestJob(t, s, j, obj, &fakeLocks{}, &fakeTokens{})
	if err := first.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n1.Sent()) != 1 {
		t.Fatalf("first notify = %v", n1.Sent())
	}
	calls := j.calls
	// 另一实例（进程内状态全新）：Tick 看到标记，不抽样、不汇总、不推送。
	second, n2 := newTestJob(t, s, j, obj, &fakeLocks{}, &fakeTokens{})
	if err := second.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n2.Sent()) != 0 || j.calls != calls {
		t.Fatalf("second instance re-ran: notify %v, judge calls %d -> %d", n2.Sent(), calls, j.calls)
	}
	// 手动补跑：日报重写，摘要不再推送。
	delete(obj.m, "eval/report/daily/20261103.html")
	out, err := second.RunOnce(t.Context())
	if err != nil || !out.Ran || out.Notified || len(n2.Sent()) != 0 {
		t.Fatalf("manual rerun = %+v, %v, notify %v; want report rewritten without notify", out, err, n2.Sent())
	}
	if _, ok := obj.m["eval/report/daily/20261103.html"]; !ok {
		t.Fatal("manual rerun must rewrite the daily report")
	}
	failing, n3 := newTestJob(t, s, j, obj, &fakeLocks{}, &fakeTokens{})
	failing.d.Objects = getErrObjects{obj}
	if err := failing.Tick(t.Context()); err == nil || len(n3.Sent()) != 0 {
		t.Fatalf("marker read failure = %v, notify %v; want error and no run", err, n3.Sent())
	}
}

// getErrObjects 读任何对象都失败（模拟 MinIO 不可用）。
type getErrObjects struct{ *memObjects }

func (getErrObjects) GetObject(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("minio down")
}

func TestJob_LockHeldSkips(t *testing.T) {
	for _, held := range []string{JobSample, JobRollup} {
		t.Run(held, func(t *testing.T) {
			s := synthDay()
			locks := &fakeLocks{held: map[string]bool{"as:test:lock:p2job:" + held: true}}
			job, n := newTestJob(t, s, &fakeJudge{}, &memObjects{}, locks, &fakeTokens{})
			out, err := job.RunOnce(t.Context())
			if err != nil || out.Ran {
				t.Fatalf("RunOnce = %+v, %v, want skipped", out, err)
			}
			if len(n.Sent()) != 0 {
				t.Fatal("notified while lock held")
			}
		})
	}
}

func TestJob_JudgeDownMarksUnjudged(t *testing.T) {
	tests := []struct {
		name   string
		judge  *fakeJudge
		tokens *fakeTokens
	}{
		{"judge error", &fakeJudge{err: errors.New("model 503")}, &fakeTokens{}},
		{"model quota exhausted", &fakeJudge{}, &fakeTokens{deny: map[string]bool{"as:test:rl:model:standard": true}}},
		{"judge qpm exhausted", &fakeJudge{}, &fakeTokens{deny: map[string]bool{"as:test:rl:judge:standard": true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := synthDay()
			job, n := newTestJob(t, s, tt.judge, &memObjects{}, &fakeLocks{}, tt.tokens)
			out, err := job.RunOnce(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if out.Fact != FactUnjudged || out.Farms[keyOf(farmA)].Fact != FactUnjudged {
				t.Fatalf("fact = %s / %s, want UNJUDGED", out.Fact, out.Farms[keyOf(farmA)].Fact)
			}
			if _, ok := platform(s, model.MetricFactConsistency); ok {
				t.Fatal("fact_consistency written while unjudged")
			}
			if !strings.Contains(n.Sent()[0], "事实一致率：离线 无数据 ｜线上 未评") {
				t.Fatalf("summary = %s", n.Sent()[0])
			}
			// judge 出错后该农场当日余下的卡不再调用（每个农场试一次）。
			if tt.judge.err != nil && tt.judge.calls != 2 {
				t.Fatalf("judge called %d times, want once per farm (2)", tt.judge.calls)
			}
		})
	}
}

// UNKNOWN 与 1 分置 NEED_EXPERT；evidence 不是原句的判定按 UNKNOWN 计。
func TestSampler_StatusMapping(t *testing.T) {
	s := synthDay()
	j := &fakeJudge{scores: map[string]*int{conclusionOf(1): nil, conclusionOf(2): ptr(1), conclusionOf(3): ptr(2)}}
	sm := &Sampler{Store: s, Cards: s, Judge: j, Version: testVersion, Tokens: &fakeTokens{}, Quota: 3, Salt: "p2l3",
		Clock: fixedClock{now}, Logger: quiet()}
	day, err := FarmDay(now, "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	res, err := sm.Sample(t.Context(), farmA, day, nil)
	if err != nil || res.Unjudged() || res.Sampled != 6 {
		t.Fatalf("Sample = %+v, %v", res, err)
	}
	status := map[uint64]string{}
	for _, r := range s.results {
		status[r.AdviceID] = r.Status
		if r.JudgeVersion != testVersion || r.StatDate != "2026-11-03" || r.RunID != 100+r.AdviceID {
			t.Fatalf("bad result %+v", r)
		}
	}
	want := map[uint64]string{1: model.JudgeStatusNeedExpert, 2: model.JudgeStatusNeedExpert, 3: model.JudgeStatusScored}
	for id, w := range want {
		if status[id] != w {
			t.Fatalf("advice %d status = %s, want %s", id, status[id], w)
		}
	}
	// evidence 不是原句：分数作废按 UNKNOWN。
	s2 := synthDay()
	bad := &badEvidenceJudge{}
	sm.Store, sm.Cards, sm.Judge = s2, s2, bad
	if _, err := sm.Sample(t.Context(), farmA, day, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range s2.results {
		if r.JudgeScore != nil || r.Status != model.JudgeStatusNeedExpert {
			t.Fatalf("unquoted evidence kept score: %+v", r)
		}
	}
}

type badEvidenceJudge struct{}

func (badEvidenceJudge) Judge(context.Context, judge.Prompt, judge.Material) (judge.Verdict, error) {
	return judge.Verdict{Score: ptr(3), Evidence: "这句话不在被评文本里出现过"}, nil
}

// UNKNOWN 占比 > 10%：不写事实一致率，日报显示「不可判」。
func TestJob_UndeterminableFact(t *testing.T) {
	s := synthDay()
	j := &fakeJudge{scores: map[string]*int{conclusionOf(1): nil}}
	job, n := newTestJob(t, s, j, &memObjects{}, &fakeLocks{}, &fakeTokens{})
	out, err := job.RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if out.Fact != FactUndeterminable {
		t.Fatalf("fact = %s, want UNDETERMINABLE (1/6 unknown)", out.Fact)
	}
	if _, ok := platform(s, model.MetricFactConsistency); ok {
		t.Fatal("fact_consistency written while undeterminable")
	}
	if !strings.Contains(n.Sent()[0], "线上 不可判") {
		t.Fatalf("summary = %s", n.Sent()[0])
	}
}

// P2-L3 修复 P2-1：rl:model 令牌已取、rl:judge 被拒或出错时退还 model 令牌；model 被拒、两令牌都取到时不退。
func TestSampler_TakeRefundsModelToken(t *testing.T) {
	const modelKey, judgeKey = "as:test:rl:model:standard", "as:test:rl:judge:standard"
	redisDown := errors.New("redis down")
	tests := []struct {
		name        string
		tokens      *fakeTokens
		wantErr     error
		wantRefunds int
	}{
		{"both granted", &fakeTokens{}, nil, 0},
		{"model denied", &fakeTokens{deny: map[string]bool{modelKey: true}}, errJudgeDown, 0},
		{"judge denied", &fakeTokens{deny: map[string]bool{judgeKey: true}}, errJudgeDown, maxTokenWaits + 1},
		{"judge error", &fakeTokens{errOn: map[string]error{judgeKey: redisDown}}, redisDown, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := &Sampler{Tokens: tt.tokens, ModelKey: modelKey, JudgeKey: judgeKey, ModelQPM: 10, JudgeQPM: 6,
				Clock: fixedClock{now}, Logger: quiet(), Wait: func(context.Context, time.Duration) error { return nil }}
			err := sm.take(t.Context())
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("take = %v, want %v", err, tt.wantErr)
			}
			if len(tt.tokens.refunds) != tt.wantRefunds {
				t.Fatalf("refunds = %v, want %d", tt.tokens.refunds, tt.wantRefunds)
			}
			for _, k := range tt.tokens.refunds {
				if k != modelKey {
					t.Fatalf("refunded %s, want only %s", k, modelKey)
				}
			}
		})
	}
}

// 专家回写：分数范围与专家号校验在 service，乐观锁冲突透传 repository.ErrJudgeRowConflict。
func TestReviewer_Validates(t *testing.T) {
	st := &reviewStore{}
	r := Reviewer{Store: st, Clock: fixedClock{now}}
	if err := r.Review(t.Context(), farmA, 1, 0, 4, 9); !errors.Is(err, ErrBadExpertScore) {
		t.Fatalf("score 4 err = %v", err)
	}
	if err := r.Review(t.Context(), farmA, 1, 0, 3, 0); err == nil {
		t.Fatal("zero expert accepted")
	}
	st.err = repository.ErrJudgeRowConflict
	if err := r.Review(t.Context(), farmA, 1, 0, 3, 9); !errors.Is(err, repository.ErrJudgeRowConflict) {
		t.Fatalf("conflict err = %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("store calls = %d, want 1", st.calls)
	}
}

type reviewStore struct {
	err   error
	calls int
}

func (s *reviewStore) ReviewJudgeResult(context.Context, model.Scope, uint64, int, int, uint64, time.Time) error {
	s.calls++
	return s.err
}
