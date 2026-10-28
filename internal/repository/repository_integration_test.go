//go:build integration

package repository_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
)

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

func TestEventAppendTx_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	ev := model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID,
		Type: model.EventOrderTransition, Actor: model.ActorSystem, Payload: []byte(`{}`), CreatedAt: t0}
	repo := repository.EventRepo{}

	var seqs []int64
	for range 2 {
		err := slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
			seq, err := repo.AppendTx(t.Context(), tx, ev)
			seqs = append(seqs, seq)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seqs = %v, want [1 2]", seqs)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM run_event WHERE run_id = ? AND seq IN (1, 2)", runID); n != 2 {
		t.Fatalf("run_event rows = %d, want 2", n)
	}

	tests := []struct {
		name    string
		run     func() error
		wantErr error
	}{
		{name: "not in tx", wantErr: repository.ErrNotInTx,
			run: func() error { _, err := repo.AppendTx(t.Context(), slot.DB, ev); return err }},
		{name: "unknown run", wantErr: repository.ErrRunNotFound, run: func() error {
			return slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
				bad := ev
				bad.RunID = runID + 999
				_, err := repo.AppendTx(t.Context(), tx, bad)
				return err
			})
		}},
		{name: "other farm", wantErr: repository.ErrRunNotFound, run: func() error {
			return slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
				bad := ev
				bad.FarmID = 2
				_, err := repo.AppendTx(t.Context(), tx, bad)
				return err
			})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSeedIdempotent_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0) // Acquire 已灌一次
	data, err := seed.Load(e.Root + "/configs/seed/fields.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Apply(t.Context(), slot.DB, data, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM field_profile WHERE tenant_id = 1 AND farm_id = 1"); n != 6 {
		t.Fatalf("field_profile rows = %d, want 6", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM field_profile WHERE row_ver = 1"); n != 6 {
		t.Fatalf("rows with row_ver=1 = %d, want 6 (second upsert bumps row_ver)", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM farm_profile"); n != 1 {
		t.Fatalf("farm_profile rows = %d, want 1", n)
	}
	id, err := repository.ProfileRepo{}.FieldIDByCode(t.Context(), slot.DB, 1, 1, "field-003")
	if err != nil || id == 0 {
		t.Fatalf("FieldIDByCode = %d, %v", id, err)
	}
	// FieldDirectory 是反查：id → field-003；不存在的 id 返回 ErrNotFound。
	dir := repository.FieldDirectory{DB: slot.DB}
	if code, err := dir.FieldCode(t.Context(), id); err != nil || code != "field-003" {
		t.Fatalf("FieldCode(%d) = %q, %v", id, code, err)
	}
	if _, err := dir.FieldCode(t.Context(), 1<<40); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("FieldCode(missing) err = %v, want ErrNotFound", err)
	}
}

// memPutter 代替 MinIO（本机 MinIO 镜像不可用，见 dev-log），只验证 raw_object 落库与幂等。
type memPutter struct {
	mu   sync.Mutex
	keys []string
}

func (m *memPutter) PutObject(_ context.Context, bucket, key string, _ []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = append(m.keys, bucket+"/"+key)
	return nil
}

func TestRawStore_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	put := &memPutter{}
	rs := repository.NewRawStore(slot.DB, put, e.Cfg.MinIO.BucketRaw)
	p := model.RawPut{TenantID: 1, FarmID: 1, RunID: 7, Kind: model.RawLLMIO, Name: "step-1.json", At: t0,
		Body: []byte(`{"a":1}`), ContentType: "application/json"}
	first, err := rs.Put(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	p.Body = []byte(`{"a":2}`)
	second, err := rs.Put(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == 0 || first.ID != second.ID || first.SHA256 == second.SHA256 {
		t.Fatalf("first=%+v second=%+v, want same id (LAST_INSERT_ID(id)) and new sha256", first, second)
	}
	if want := "raw/1/1/2026/07/15/7/LLM_IO/step-1.json"; first.ObjectKey != want || !strings.HasSuffix(put.keys[0], want) {
		t.Fatalf("key = %s (put %v), want %s", first.ObjectKey, put.keys, want)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM raw_object WHERE sha256 = ?", second.SHA256); n != 1 {
		t.Fatalf("raw_object rows = %d, want 1", n)
	}
}

func TestLocker_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	l := repository.NewLocker(e.Redis)
	key := slot.Keys.LockOrder(42)
	if !strings.HasPrefix(key, "as:it-repo-t1:lock:order:") {
		t.Fatalf("key = %s", key)
	}
	ctx := t.Context()
	lk, err := l.Acquire(ctx, key, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(ctx, key, 10*time.Second); !errors.Is(err, repository.ErrLockHeld) {
		t.Fatalf("second acquire err = %v, want ErrLockHeld", err)
	}
	if err := lk.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(ctx); !errors.Is(err, repository.ErrLockLost) {
		t.Fatalf("double release err = %v, want ErrLockLost", err)
	}
	// 他人持有时，旧 token 不能释放新锁
	if _, err := l.Acquire(ctx, key, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := lk.Renew(ctx); !errors.Is(err, repository.ErrLockLost) {
		t.Fatalf("stale renew err = %v, want ErrLockLost", err)
	}
}

func TestPoolResetsPrefixAndClock_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	ctx := t.Context()
	slot, err := e.Pool.Acquire(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	key := slot.Keys.LockScan("expire")
	if err := e.Redis.Set(ctx, key, "x", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	itest.InsertRun(t, slot.DB, t0)
	e.Pool.Release(slot)

	later := t0.Add(24 * time.Hour)
	slot, err = e.Pool.Acquire(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Pool.Release(slot)
	if n, err := e.Redis.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("exists(%s) = %d, %v; want key cleared on Acquire", key, n, err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM agent_run"); n != 0 {
		t.Fatalf("agent_run rows = %d, want truncated", n)
	}
	if !slot.Clock.Now().Equal(later) {
		t.Fatalf("clock = %v, want %v", slot.Clock.Now(), later)
	}
}
