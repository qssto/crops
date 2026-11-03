package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// memRetention 是内存版保留期仓储 + 事务 + 审计 + 对象存储：事务内的墓碑与审计先暂存，fn 成功才落（模拟回滚）。
type memRetention struct {
	rows      []repository.ExpiredRaw
	refs      map[uint64]bool
	refErr    error
	auditErr  error
	removeErr map[string]error
	removed   []string
	deleted   map[uint64]bool
	audits    []model.AuditRecord
	lists     []repository.RetentionCursor

	stageDel   []uint64
	stageAudit []model.AuditRecord
}

func (m *memRetention) ListExpired(_ context.Context, _ repository.SystemScope, now time.Time, after repository.RetentionCursor,
	limit int) ([]repository.ExpiredRaw, error) {
	m.lists = append(m.lists, after)
	var out []repository.ExpiredRaw
	for _, r := range m.rows {
		past := r.ExpireAt.After(after.ExpireAt) || r.ExpireAt.Equal(after.ExpireAt) && r.ID > after.ID
		if !m.deleted[r.ID] && !r.ExpireAt.After(now) && past && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memRetention) Referenced(_ context.Context, _ repository.SystemScope, ids []uint64) (map[uint64]bool, error) {
	out := map[uint64]bool{}
	for _, id := range ids {
		if m.refs[id] {
			out[id] = true
		}
	}
	return out, m.refErr
}

func (m *memRetention) MarkDeletedTx(_ context.Context, _ *repository.DB, ref repository.RawRef, _ time.Time) (bool, error) {
	if m.deleted[ref.ID] || slices.Contains(m.stageDel, ref.ID) {
		return false, nil
	}
	m.stageDel = append(m.stageDel, ref.ID)
	return true, nil
}

func (m *memRetention) AppendTx(_ context.Context, _ *repository.DB, r model.AuditRecord) error {
	if m.auditErr != nil {
		return m.auditErr
	}
	m.stageAudit = append(m.stageAudit, r)
	return nil
}

func (m *memRetention) InTx(_ context.Context, fn func(*repository.DB) error) error {
	m.stageDel, m.stageAudit = nil, nil
	if err := fn(nil); err != nil {
		return err
	}
	for _, id := range m.stageDel {
		m.deleted[id] = true
	}
	m.audits = append(m.audits, m.stageAudit...)
	return nil
}

func (m *memRetention) RemoveObject(_ context.Context, _, key string) error {
	if err := m.removeErr[key]; err != nil {
		return err
	}
	m.removed = append(m.removed, key)
	return nil
}

// retentionNow 是 2026-11-02 北京时间 03:00。
var retentionNow = time.Date(2026, 11, 1, 19, 0, 0, 0, time.UTC)

func newRetentionWorld(n int) *memRetention {
	m := &memRetention{refs: map[uint64]bool{}, removeErr: map[string]error{}, deleted: map[uint64]bool{}}
	for i := 1; i <= n; i++ {
		farm := uint64(1 + i%2)
		m.rows = append(m.rows, repository.ExpiredRaw{RawRef: repository.RawRef{TenantID: 1, FarmID: farm, ID: uint64(i)},
			Kind: model.RawLLMIO, ObjectKey: fmt.Sprintf("k/%d", i), ExpireAt: retentionNow.Add(-time.Duration(n-i+1) * time.Hour)})
	}
	return m
}

func (m *memRetention) service(batch int) *RetentionService {
	return &RetentionService{Store: m, Tx: m, Audit: m, Objects: m, Bucket: "raw", Logger: discardLogger(), BatchSize: batch}
}

func TestRetentionTick(t *testing.T) {
	t.Run("purges, skips referenced, audits per farm", func(t *testing.T) {
		m := newRetentionWorld(5)
		m.refs[2] = true // 未终态工单 / 未关闭票据引用
		st, err := m.service(2).Tick(t.Context(), scanSystem, retentionNow)
		if err != nil {
			t.Fatal(err)
		}
		if st.Purged != 4 || st.Referenced != 1 || !st.Drained || m.deleted[2] || slices.Contains(m.removed, "k/2") {
			t.Fatalf("stats %+v deleted %v removed %v", st, m.deleted, m.removed)
		}
		for _, a := range m.audits {
			if a.Action != string(model.AuditRetentionPurge) || a.Actor.Type != model.ActorSystem || a.Target != "raw_kind:LLM_IO" {
				t.Fatalf("audit %+v", a)
			}
		}
		if len(m.audits) < 2 {
			t.Fatalf("want per-(tenant,farm) audits, got %d", len(m.audits))
		}
	})
	t.Run("remove failure keeps row for retry", func(t *testing.T) {
		m := newRetentionWorld(3)
		m.removeErr["k/1"] = errors.New("minio down")
		st, err := m.service(10).Tick(t.Context(), scanSystem, retentionNow)
		if err == nil || st.Failed != 1 || st.Purged != 2 || m.deleted[1] {
			t.Fatalf("err %v stats %+v deleted %v", err, st, m.deleted)
		}
	})
	t.Run("audit failure rolls back tombstones and cursor stays", func(t *testing.T) {
		m := newRetentionWorld(3)
		m.auditErr = errors.New("audit down")
		s := m.service(10)
		if _, err := s.Tick(t.Context(), scanSystem, retentionNow); !errors.Is(err, errRetentionFatal) {
			t.Fatalf("err = %v, want errRetentionFatal", err)
		}
		if len(m.deleted) != 0 || len(m.audits) != 0 {
			t.Fatalf("tombstones committed without audit: %v %v", m.deleted, m.audits)
		}
		m.auditErr = nil
		st, err := s.Tick(t.Context(), scanSystem, retentionNow)
		if err != nil || st.Purged != 3 {
			t.Fatalf("retry: err %v stats %+v (cursor must not advance past failed batch)", err, st)
		}
	})
	t.Run("reference query failure is fatal", func(t *testing.T) {
		m := newRetentionWorld(2)
		m.refErr = errors.New("db down")
		if _, err := m.service(10).Tick(t.Context(), scanSystem, retentionNow); !errors.Is(err, errRetentionFatal) || len(m.removed) != 0 {
			t.Fatalf("err %v removed %v", err, m.removed)
		}
	})
	t.Run("drained day is not rescanned", func(t *testing.T) {
		m := newRetentionWorld(1)
		s := m.service(10)
		for range 2 {
			if _, err := s.Tick(t.Context(), scanSystem, retentionNow); err != nil {
				t.Fatal(err)
			}
		}
		if len(m.lists) != 1 {
			t.Fatalf("lists = %d, want 1", len(m.lists))
		}
		if _, err := s.Tick(t.Context(), scanSystem, retentionNow.Add(24*time.Hour)); err != nil || len(m.lists) != 2 {
			t.Fatalf("next day: err %v lists %d", err, len(m.lists))
		}
	})
}
