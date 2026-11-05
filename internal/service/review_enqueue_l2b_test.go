package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type fakeEnqueueSource struct {
	farms []repository.FarmTZ
	rows  map[uint64][]model.OnlineJudgeResult // farm → NEED_EXPERT 行（id 升序）
	pages int
}

func (f *fakeEnqueueSource) FarmTimezones(context.Context, repository.SystemScope) ([]repository.FarmTZ, error) {
	return f.farms, nil
}

func (f *fakeEnqueueSource) NeedExpertResults(_ context.Context, scope model.Scope, afterID uint64, limit int) ([]model.OnlineJudgeResult, error) {
	f.pages++
	var out []model.OnlineJudgeResult
	for _, r := range f.rows[scope.FarmID] {
		if r.ID > afterID && len(out) < limit {
			r.TenantID, r.FarmID = scope.TenantID, scope.FarmID
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeEnqueueSink struct {
	got  []model.ReviewTask
	seen map[string]bool
	err  error
}

func (f *fakeEnqueueSink) InsertReviewTasks(_ context.Context, _ repository.SystemScope, tasks []model.ReviewTask, _ time.Time) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n := 0
	for _, t := range tasks {
		if !f.seen[t.Key()] {
			f.seen[t.Key()] = true
			f.got = append(f.got, t)
			n++
		}
	}
	return n, nil
}

func newEnqueue(t *testing.T, src *fakeEnqueueSource, sink *fakeEnqueueSink, locks KeyLocker) *ReviewEnqueue {
	t.Helper()
	j, err := NewReviewEnqueue(ReviewEnqueueDeps{Source: src, Sink: sink, Locks: locks, LockKey: "lock:p2job:" + ReviewEnqueueJob,
		Clock: fixedClock{reviewT0}, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Batch: 2,
		System: repository.NewSystemScope(ReviewEnqueueJob)})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// 逐农场按 id 分页把 NEED_EXPERT 行入队为 ONLINE_JUDGE（draft_ref 带本行 tenant / farm），重跑不重复；锁被持有跳过。
func TestReviewEnqueue_Tick(t *testing.T) {
	one := 1
	row := func(id uint64) model.OnlineJudgeResult {
		return model.OnlineJudgeResult{ID: id, Dimension: "fact", JudgeVersion: "j1", JudgeScore: &one, Status: model.JudgeStatusNeedExpert}
	}
	src := &fakeEnqueueSource{farms: []repository.FarmTZ{{TenantID: 1, FarmID: 1}, {TenantID: 2, FarmID: 5}},
		rows: map[uint64][]model.OnlineJudgeResult{1: {row(3), row(4), row(9)}, 5: {row(10)}}}
	sink := &fakeEnqueueSink{seen: map[string]bool{}}
	locks := &kbLocks{lease: &kbLease{}}
	j := newEnqueue(t, src, sink, locks)
	n, err := j.Tick(t.Context())
	if err != nil || n != 4 || len(sink.got) != 4 {
		t.Fatalf("tick = %d, %v (got %d)", n, err, len(sink.got))
	}
	if locks.key != "lock:p2job:"+ReviewEnqueueJob || locks.lease.releases != 1 || locks.lease.renews != 2 {
		t.Fatalf("lock key %q renews %d releases %d", locks.key, locks.lease.renews, locks.lease.releases)
	}
	last := sink.got[3]
	if last.TaskType != model.ReviewOnlineJudge || last.DraftRef != model.OnlineJudgeRef(2, 5, 10) || last.Dimension != "fact" ||
		last.JudgeVersion != "j1" || last.JudgeScore == nil || *last.JudgeScore != 1 || last.ExperimentID != nil || last.SampleID != "" {
		t.Fatalf("task = %+v", last)
	}
	if n, err := j.Tick(t.Context()); err != nil || n != 0 {
		t.Fatalf("re-tick = %d, %v", n, err)
	}
	held := newEnqueue(t, src, sink, &kbLocks{err: fmt.Errorf("x: %w", repository.ErrLockHeld)})
	before := src.pages
	if n, err := held.Tick(t.Context()); err != nil || n != 0 || src.pages != before {
		t.Fatalf("held tick = %d, %v pages %d→%d", n, err, before, src.pages)
	}
	broken := newEnqueue(t, src, &fakeEnqueueSink{err: errors.New("db down")}, nil)
	if _, err := broken.Tick(t.Context()); err == nil {
		t.Fatal("sink error must surface")
	}
	if _, err := NewReviewEnqueue(ReviewEnqueueDeps{}); err == nil {
		t.Fatal("NewReviewEnqueue must reject missing deps")
	}
}

type fakeEnqueueCursor struct {
	at      map[uint64]uint64 // farm → after
	resets  int
	loadErr error
}

func (f *fakeEnqueueCursor) EnqueueCursor(_ context.Context, s model.Scope) (uint64, error) {
	return f.at[s.FarmID], f.loadErr
}

func (f *fakeEnqueueCursor) SetEnqueueCursor(_ context.Context, s model.Scope, after uint64) error {
	f.at[s.FarmID] = after
	return nil
}

func (f *fakeEnqueueCursor) ResetEnqueueCursor(_ context.Context, s model.Scope) error {
	delete(f.at, s.FarmID)
	f.resets++
	return nil
}

// P2-3：单农场单轮读满 reviewEnqueueMaxPagesPerFarm 页时记游标，下一轮从游标续扫到末尾（不再卡在前 50 页）；
// 扫到末尾删游标，再下一轮从头扫（已入队行靠去重跳过）；读游标失败该轮报错、不读业务库。
func TestReviewEnqueue_CursorResumes(t *testing.T) {
	one := 1
	perRound := reviewEnqueueMaxPagesPerFarm * 2 // Batch 2
	var rows []model.OnlineJudgeResult
	for id := uint64(1); id <= uint64(perRound+3); id++ {
		rows = append(rows, model.OnlineJudgeResult{ID: id, Dimension: "fact", JudgeVersion: "j1", JudgeScore: &one})
	}
	src := &fakeEnqueueSource{farms: []repository.FarmTZ{{TenantID: 1, FarmID: 1}}, rows: map[uint64][]model.OnlineJudgeResult{1: rows}}
	sink := &fakeEnqueueSink{seen: map[string]bool{}}
	cur := &fakeEnqueueCursor{at: map[uint64]uint64{}}
	j := newEnqueue(t, src, sink, nil)
	j.d.Cursor = cur
	if n, err := j.Tick(t.Context()); err != nil || n != perRound || cur.at[1] != uint64(perRound) {
		t.Fatalf("round 1 = %d, %v cursor %v", n, err, cur.at)
	}
	if n, err := j.Tick(t.Context()); err != nil || n != 3 || len(cur.at) != 0 || cur.resets != 1 {
		t.Fatalf("round 2 = %d, %v cursor %v resets %d", n, err, cur.at, cur.resets)
	}
	if last := sink.got[len(sink.got)-1]; last.DraftRef != model.OnlineJudgeRef(1, 1, uint64(perRound+3)) {
		t.Fatalf("tail row not enqueued: %+v", last)
	}
	before := src.pages
	if n, err := j.Tick(t.Context()); err != nil || n != 0 || src.pages-before != reviewEnqueueMaxPagesPerFarm {
		t.Fatalf("round 3 (from start) = %d, %v pages %d", n, err, src.pages-before)
	}
	cur.loadErr = errors.New("redis down")
	before = src.pages
	if _, err := j.Tick(t.Context()); !errors.Is(err, cur.loadErr) || src.pages != before {
		t.Fatalf("cursor load err = %v pages %d→%d", err, before, src.pages)
	}
}
