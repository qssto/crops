package attribution

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"slices"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stepClock 是可拨动的注入时钟（4 周模拟用）。
type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time { return c.t }

// fakeHits 按农场返回预置信号（取 [from, to) 内的）；calls 记录被查询的农场。
type fakeHits struct {
	hits  []model.AttributionHit
	flow  map[uint64]map[string]int
	calls []uint64
}

func (f *fakeHits) WeekHits(_ context.Context, scope model.Scope, from, to time.Time) ([]model.AttributionHit, error) {
	f.calls = append(f.calls, scope.FarmID)
	var out []model.AttributionHit
	for _, h := range f.hits {
		if h.TenantID == scope.TenantID && h.FarmID == scope.FarmID && !h.At.Before(from) && h.At.Before(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (f *fakeHits) BackflowCounts(_ context.Context, scope model.Scope, _, _ time.Time) (map[string]int, error) {
	return f.flow[scope.FarmID], nil
}

type fakeFarms []repository.FarmTZ

func (f fakeFarms) FarmTimezones(context.Context, repository.SystemScope) ([]repository.FarmTZ, error) {
	return f, nil
}

// fakeIssues 模拟 attribution_issue upsert（照 ② 的 ON DUPLICATE KEY UPDATE 语义）。
type fakeIssues struct {
	rows   map[string]*model.AttributionIssue
	nextID uint64
}

func (f *fakeIssues) IssuesBySignature(_ context.Context, _ repository.SystemScope, sigs []string) ([]model.AttributionIssue, error) {
	var out []model.AttributionIssue
	for _, s := range sigs {
		if r, ok := f.rows[s]; ok {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeIssues) WeekIssues(_ context.Context, _ repository.SystemScope, week string, limit int) ([]model.AttributionIssue, error) {
	var out []model.AttributionIssue
	for _, r := range f.rows {
		if r.LastWeek == week {
			out = append(out, *r)
		}
	}
	slices.SortFunc(out, func(a, b model.AttributionIssue) int {
		return cmp.Or(cmp.Compare(b.HitCount, a.HitCount), cmp.Compare(a.Signature, b.Signature))
	})
	return out[:min(limit, len(out))], nil
}

func (f *fakeIssues) UpsertIssues(_ context.Context, _ repository.SystemScope, rows []model.IssueUpsert, at time.Time) error {
	if f.rows == nil {
		f.rows = map[string]*model.AttributionIssue{}
	}
	for _, u := range rows {
		r, ok := f.rows[u.Signature]
		if !ok {
			f.nextID++
			kb := u.PrimaryKB
			f.rows[u.Signature] = &model.AttributionIssue{ID: f.nextID, Signature: u.Signature, FirstWeek: u.Week, LastWeek: u.Week,
				Reason: u.Reason, QuestionType: u.QuestionType, PrimaryKB: &kb, HitCount: u.HitCount, Evidence: u.Evidence,
				Status: model.IssueOpen, CreatedAt: at, UpdatedAt: at}
			continue
		}
		r.LastWeek, r.HitCount, r.Evidence = u.Week, u.HitCount, u.Evidence
		if r.Closed() {
			r.ReopenCount++
			r.Status = model.IssueOpen
		}
		r.RowVer++
		r.UpdatedAt = at
	}
	return nil
}

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

type fakeLocks struct {
	keys      []string
	held      map[string]bool
	renews    int
	onAcquire func() // 取锁成功前回调（模拟另一副本在本副本等锁期间已出完周报）
}

func (l *fakeLocks) AcquireLease(_ context.Context, key string, _ time.Duration) (agent.Lease, error) {
	l.keys = append(l.keys, key)
	if l.held[key] {
		return nil, repository.ErrLockHeld
	}
	if l.onAcquire != nil {
		l.onAcquire()
	}
	return fakeLease{renews: &l.renews}, nil
}
