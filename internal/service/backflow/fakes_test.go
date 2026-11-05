package backflow

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type fakeFarms []repository.FarmTZ

func (f fakeFarms) FarmTimezones(context.Context, repository.SystemScope) ([]repository.FarmTZ, error) {
	return f, nil
}

// fakeHits 按 Scope 返回预置信号（故意不过滤农场：验证任务自己按 Scope 防串场）。
type fakeHits struct {
	byFarm map[uint64][]model.AttributionHit
	calls  []model.Scope
}

func (f *fakeHits) WeekHits(_ context.Context, scope model.Scope, _, _ time.Time) ([]model.AttributionHit, error) {
	f.calls = append(f.calls, scope)
	return f.byFarm[scope.FarmID], nil
}

type runKey struct{ tenant, farm, run uint64 }

// fakeStore 模拟 agent_run.backflow_status / backflow_draft_ref（按 tenant / farm / run 归属）。
type fakeStore struct {
	status    map[runKey]string
	drafts    map[runKey]string
	materials map[runKey]model.BackflowMaterial
	marked    []runKey
	attempts  map[runKey]int
	markCalls []markCall // MarkSelected 入参（验证不把他场 run 交给本场 Scope）
}

type markCall struct {
	scope model.Scope
	ids   []uint64
}

func newFakeStore() *fakeStore {
	return &fakeStore{status: map[runKey]string{}, drafts: map[runKey]string{}, materials: map[runKey]model.BackflowMaterial{},
		attempts: map[runKey]int{}}
}

func (f *fakeStore) addRun(tenant, farm uint64, m model.BackflowMaterial) {
	k := runKey{tenant, farm, m.RunID}
	f.status[k] = ""
	f.materials[k] = m
}

func (f *fakeStore) MarkSelected(_ context.Context, s model.Scope, ids []uint64, _ time.Time) (int64, error) {
	f.markCalls = append(f.markCalls, markCall{scope: s, ids: slices.Clone(ids)})
	var n int64
	for _, id := range ids {
		k := runKey{s.TenantID, s.FarmID, id}
		if st, ok := f.status[k]; ok && st == "" {
			f.status[k] = model.BackflowSelected
			f.marked = append(f.marked, k)
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) SelectedRuns(_ context.Context, s model.Scope, limit int) ([]uint64, error) {
	var ids []uint64
	for k, st := range f.status {
		if k.tenant == s.TenantID && k.farm == s.FarmID && st == model.BackflowSelected {
			ids = append(ids, k.run)
		}
	}
	slices.Sort(ids)
	return ids[:min(limit, len(ids))], nil
}

func (f *fakeStore) Material(_ context.Context, s model.Scope, runID uint64) (model.BackflowMaterial, error) {
	m, ok := f.materials[runKey{s.TenantID, s.FarmID, runID}]
	if !ok {
		return model.BackflowMaterial{}, model.ErrNotFound
	}
	return m, nil
}

func (f *fakeStore) FinishDraft(_ context.Context, s model.Scope, runID uint64, status, ref string, _ time.Time) error {
	k := runKey{s.TenantID, s.FarmID, runID}
	if f.status[k] != model.BackflowSelected {
		return model.ErrConcurrentUpdate
	}
	f.status[k], f.drafts[k] = status, ref
	return nil
}

func (f *fakeStore) FailAttempt(_ context.Context, s model.Scope, runID uint64, maxAttempts int, _ time.Time) (bool, error) {
	k := runKey{s.TenantID, s.FarmID, runID}
	if f.status[k] != model.BackflowSelected {
		return false, model.ErrConcurrentUpdate
	}
	f.attempts[k]++
	if f.attempts[k] >= maxAttempts {
		f.status[k] = model.BackflowFailed
		return true, nil
	}
	return false, nil
}

// fakeRaw 模拟 AuditService.ReadRaw：每次调用记一条 RAW_READ（即审计条数），只认本 Scope 本 run 的原文。
type fakeRaw struct {
	objects map[runKey]map[uint64][]byte // run → raw_id → body
	reads   []rawRead
	expired map[uint64]bool
}

type rawRead struct {
	scope        model.Scope
	runID, rawID uint64
}

func (f *fakeRaw) ReadRaw(_ context.Context, s model.Scope, runID, rawID uint64) (service.RawLink, error) {
	f.reads = append(f.reads, rawRead{scope: s, runID: runID, rawID: rawID})
	if f.expired[rawID] {
		return service.RawLink{}, fmt.Errorf("raw %d: %w", rawID, model.ErrRawExpired)
	}
	if _, ok := f.objects[runKey{s.TenantID, s.FarmID, runID}][rawID]; !ok {
		return service.RawLink{}, fmt.Errorf("raw %d: %w", rawID, service.ErrForbidden)
	}
	return service.RawLink{RawID: rawID, URL: fmt.Sprintf("raw://%d/%d/%d/%d", s.TenantID, s.FarmID, runID, rawID)}, nil
}

// Fetch 按 ReadRaw 签发的 URL 取正文（未经 ReadRaw 的 URL 取不到）。
func (f *fakeRaw) Fetch(_ context.Context, url string) ([]byte, error) {
	var t, fa, r, id uint64
	if _, err := fmt.Sscanf(url, "raw://%d/%d/%d/%d", &t, &fa, &r, &id); err != nil {
		return nil, err
	}
	body, ok := f.objects[runKey{t, fa, r}][id]
	if !ok {
		return nil, model.ErrNotFound
	}
	return body, nil
}

type fakeLease struct{ renews *int }

func (l fakeLease) Renew(context.Context) error   { *l.renews++; return nil }
func (l fakeLease) Release(context.Context) error { return nil }

type fakeLocks struct {
	keys   []string
	held   bool
	renews int
}

func (l *fakeLocks) AcquireLease(_ context.Context, key string, _ time.Duration) (agent.Lease, error) {
	l.keys = append(l.keys, key)
	if l.held {
		return nil, repository.ErrLockHeld
	}
	return fakeLease{renews: &l.renews}, nil
}
