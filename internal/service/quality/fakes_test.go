package quality

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service/quality/judge"
)

// fakeStore 是内存版 repository.QualityRepo（单测用）：按窗口过滤合成卡，judge 结果与日指标按唯一键覆盖。
type fakeStore struct {
	mu      sync.Mutex
	farms   []repository.FarmTZ
	cards   []fakeCard
	results []model.OnlineJudgeResult
	daily   map[string]model.QualityDaily
	upserts int
}

type fakeCard struct {
	scope    model.Scope
	id, run  uint64
	at       time.Time
	category string
	outcome  model.Outcome
	reason   string
	degraded bool
	cited    bool
}

func newFakeStore() *fakeStore { return &fakeStore{daily: map[string]model.QualityDaily{}} }

func (f *fakeStore) in(c fakeCard, scope model.Scope, from, to time.Time) bool {
	return c.scope.TenantID == scope.TenantID && c.scope.FarmID == scope.FarmID && !c.at.Before(from) && c.at.Before(to)
}

func (f *fakeStore) FarmTimezones(context.Context, repository.SystemScope) ([]repository.FarmTZ, error) {
	return f.farms, nil
}

func (f *fakeStore) AdviceOutcomeCounts(_ context.Context, scope model.Scope, from, to time.Time) ([]repository.OutcomeCount, error) {
	type key struct {
		o, r string
		d    bool
	}
	n := map[key]int{}
	var order []key
	for _, c := range f.cards {
		if !f.in(c, scope, from, to) {
			continue
		}
		k := key{string(c.outcome), c.reason, c.degraded}
		if n[k] == 0 {
			order = append(order, k)
		}
		n[k]++
	}
	var out []repository.OutcomeCount
	for _, k := range order {
		oc := repository.OutcomeCount{Outcome: k.o, Degraded: k.d, N: n[k]}
		if k.r != "" {
			oc.RefuseReason = ptr(k.r)
		}
		out = append(out, oc)
	}
	return out, nil
}

func (f *fakeStore) CitedAdviceCount(_ context.Context, scope model.Scope, from, to time.Time) (int, error) {
	n := 0
	for _, c := range f.cards {
		if f.in(c, scope, from, to) && c.outcome == model.OutcomeAdvice && c.cited {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) tally(match func(model.OnlineJudgeResult) bool, top int) repository.JudgeTally {
	var t repository.JudgeTally
	for _, r := range f.results {
		if !match(r) {
			continue
		}
		switch s := r.Score(); {
		case s == nil:
			t.Unknown++
		case *s == top:
			t.Scored++
			t.Top++
		default:
			t.Scored++
		}
	}
	return t
}

func (f *fakeStore) FarmJudgeTally(_ context.Context, scope model.Scope, date, dim, version string, top int) (repository.JudgeTally, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tally(func(r model.OnlineJudgeResult) bool {
		return r.TenantID == scope.TenantID && r.FarmID == scope.FarmID && r.StatDate == date && r.Dimension == dim &&
			r.JudgeVersion == version
	}, top), nil
}

func (f *fakeStore) PlatformJudgeTally(_ context.Context, _ repository.SystemScope, date, dim, version string, top int) (repository.JudgeTally, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tally(func(r model.OnlineJudgeResult) bool {
		return r.StatDate == date && r.Dimension == dim && r.JudgeVersion == version
	}, top), nil
}

func dailyKey(q model.QualityDaily) string {
	return fmt.Sprintf("%d/%d|%s|%s|%s|%s", q.TenantID, q.FarmID, q.StatDate, q.Source, q.Metric, q.SliceKey)
}

func (f *fakeStore) OnlineFarmTotals(_ context.Context, _ repository.SystemScope, date string) ([]repository.MetricTotal, error) {
	sum := map[string]*repository.MetricTotal{}
	var order []string
	for _, q := range f.daily {
		if q.StatDate != date || q.Source != model.MetricSourceOnline || q.SliceKey != model.SliceAll || (q.TenantID == 0 && q.FarmID == 0) {
			continue
		}
		t, ok := sum[q.Metric]
		if !ok {
			t = &repository.MetricTotal{Metric: q.Metric}
			sum[q.Metric] = t
			order = append(order, q.Metric)
		}
		t.Numerator += q.Numerator
		t.Denominator += q.Denominator
	}
	var out []repository.MetricTotal
	for _, m := range order {
		out = append(out, *sum[m])
	}
	return out, nil
}

func (f *fakeStore) UpsertDaily(_ context.Context, _ repository.SystemScope, rows []model.QualityDaily, _ time.Time) error {
	f.upserts++
	for _, q := range rows {
		f.daily[dailyKey(q)] = q
	}
	return nil
}

func (f *fakeStore) DailyRows(_ context.Context, _ repository.SystemScope, tenantID, farmID uint64, date string) ([]model.QualityDaily, error) {
	var out []model.QualityDaily
	for _, q := range f.daily {
		if q.TenantID == tenantID && q.FarmID == farmID && q.StatDate == date {
			out = append(out, q)
		}
	}
	return out, nil
}

func (f *fakeStore) AdviceRuns(_ context.Context, scope model.Scope, from, to time.Time, flt repository.RunFilter, limit int) ([]uint64, error) {
	var out []uint64
	for _, c := range f.cards {
		if !f.in(c, scope, from, to) || len(out) >= limit {
			continue
		}
		hit := false
		switch flt {
		case repository.RunsUncited:
			hit = c.outcome == model.OutcomeAdvice && !c.cited
		case repository.RunsValidationFailed:
			hit = c.outcome == model.OutcomeRefused && c.reason == RefuseValidation
		case repository.RunsRefused:
			hit = c.outcome == model.OutcomeRefused
		}
		if hit {
			out = append(out, c.run)
		}
	}
	return out, nil
}

func (f *fakeStore) AdviceCategories(_ context.Context, scope model.Scope, from, to time.Time) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, c := range f.cards {
		if f.in(c, scope, from, to) && c.outcome == model.OutcomeAdvice && !seen[c.category] {
			seen[c.category] = true
			out = append(out, c.category)
		}
	}
	return out, nil
}

func (f *fakeStore) SampleAdvice(_ context.Context, scope model.Scope, cat string, from, to time.Time, _ string, limit int) ([]repository.AdviceRef, error) {
	var out []repository.AdviceRef
	for _, c := range f.cards {
		if f.in(c, scope, from, to) && c.outcome == model.OutcomeAdvice && c.category == cat && len(out) < limit {
			out = append(out, repository.AdviceRef{ID: c.id, RunID: c.run})
		}
	}
	return out, nil
}

func (f *fakeStore) JudgeResults(_ context.Context, scope model.Scope, date, dim string) ([]model.OnlineJudgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.OnlineJudgeResult
	for _, r := range f.results {
		if r.TenantID == scope.TenantID && r.FarmID == scope.FarmID && r.StatDate == date && r.Dimension == dim {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) InsertJudgeResult(_ context.Context, _ model.Scope, j model.OnlineJudgeResult, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.results {
		if r.TenantID == j.TenantID && r.FarmID == j.FarmID && r.AdviceID == j.AdviceID && r.Dimension == j.Dimension &&
			r.JudgeVersion == j.JudgeVersion {
			return false, nil
		}
	}
	j.ID = uint64(len(f.results) + 1)
	f.results = append(f.results, j)
	return true, nil
}

func (f *fakeStore) RunKnowledgeRef(context.Context, model.Scope, uint64) (string, error) {
	return "kb@test", nil
}

func (f *fakeStore) GetByRun(_ context.Context, scope model.Scope, runID uint64) (model.AdviceCard, error) {
	for _, c := range f.cards {
		if c.run == runID && c.scope.FarmID == scope.FarmID {
			return model.AdviceCard{AdviceID: c.id, RunID: c.run, TenantID: scope.TenantID, FarmID: scope.FarmID,
				Category: model.Category(c.category), Outcome: c.outcome, Conclusion: conclusionOf(c.id),
				Reasons: []string{"理由"}}, nil
		}
	}
	return model.AdviceCard{}, model.ErrNotFound
}

func conclusionOf(adviceID uint64) string {
	return fmt.Sprintf("建议对第 %d 号地块按规则层剂量作业", adviceID)
}

// fakeJudge 按卡回放分数：scores[conclusionOf(advice_id)]，缺省 3（nil 值为 UNKNOWN）；err 非 nil 模拟 judge 不可用。
type fakeJudge struct {
	scores map[string]*int
	err    error
	calls  int
}

func (j *fakeJudge) Judge(_ context.Context, _ judge.Prompt, m judge.Material) (judge.Verdict, error) {
	j.calls++
	if j.err != nil {
		return judge.Verdict{}, j.err
	}
	s, ok := j.scores[m.Output.Conclusion]
	if !ok {
		s = ptr(3)
	}
	return judge.Verdict{Score: s, Evidence: m.Output.Conclusion}, nil
}

// fakeTokens 记录取令牌的键序；deny 中的键返回 false。
type fakeTokens struct {
	keys    []string
	refunds []string
	deny    map[string]bool
	errOn   map[string]error // Take 该键时返回的错误
}

func (t *fakeTokens) Take(_ context.Context, key string, _ int, _ time.Time) (bool, error) {
	t.keys = append(t.keys, key)
	if err := t.errOn[key]; err != nil {
		return false, err
	}
	return !t.deny[key], nil
}

func (t *fakeTokens) Refund(_ context.Context, key string, _ int, _ time.Time) error {
	t.refunds = append(t.refunds, key)
	return nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// memObjects 是内存对象存储；不存在返回 model.ErrNotFound。
type memObjects struct{ m map[string][]byte }

func (o *memObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	b, ok := o.m[bucket+"/"+key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func (o *memObjects) PutObject(_ context.Context, bucket, key string, body []byte, _ string) error {
	if o.m == nil {
		o.m = map[string][]byte{}
	}
	o.m[bucket+"/"+key] = body
	return nil
}

type fakeLease struct{ renews *int }

func (l fakeLease) Renew(context.Context) error   { *l.renews++; return nil }
func (l fakeLease) Release(context.Context) error { return nil }

// fakeLocks 记录取锁的键；held 中的键返回 ErrLockHeld。
type fakeLocks struct {
	keys   []string
	held   map[string]bool
	renews int
}

func (l *fakeLocks) AcquireLease(_ context.Context, key string, _ time.Duration) (agent.Lease, error) {
	l.keys = append(l.keys, key)
	if l.held[key] {
		return nil, repository.ErrLockHeld
	}
	return fakeLease{renews: &l.renews}, nil
}
