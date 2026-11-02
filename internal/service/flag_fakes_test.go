package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

var flagNow = time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)

// memFlagRow 是 feature_flag 一行的内存模型。
type memFlagRow struct {
	st     model.FlagScopeType
	value  json.RawMessage
	rowVer int
}

type flagLoc struct {
	key              model.FlagKey
	tenantID, farmID uint64
}

// memFlagStore 是 FlagStore + TxRunner + AuditTxAppender 的内存实现；InTx 失败时回滚行与审计。
type memFlagStore struct {
	mu        sync.Mutex
	rows      map[flagLoc]memFlagRow
	farms     map[[2]uint64]bool
	audits    []model.AuditRecord
	reads     int
	readErr   error
	auditFail error
	writeFail error
}

func newMemFlagStore(farms ...[2]uint64) *memFlagStore {
	m := &memFlagStore{rows: map[flagLoc]memFlagRow{}, farms: map[[2]uint64]bool{}}
	for _, f := range farms {
		m.farms[f] = true
	}
	return m
}

func (m *memFlagStore) put(key model.FlagKey, t, f uint64, value string) {
	st, err := model.FlagScopeOf(t, f)
	if err != nil {
		panic(err)
	}
	m.rows[flagLoc{key, t, f}] = memFlagRow{st: st, value: json.RawMessage(value)}
}

func (m *memFlagStore) InTx(_ context.Context, fn func(tx *repository.DB) error) error {
	m.mu.Lock()
	rows := make(map[flagLoc]memFlagRow, len(m.rows))
	for k, v := range m.rows {
		rows[k] = v
	}
	n := len(m.audits)
	m.mu.Unlock()
	if err := fn(nil); err != nil {
		m.mu.Lock()
		m.rows, m.audits = rows, m.audits[:n]
		m.mu.Unlock()
		return err
	}
	return nil
}

func (m *memFlagStore) Rows(_ context.Context, key model.FlagKey, t, f uint64) ([]model.FlagRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	if m.readErr != nil {
		return nil, m.readErr
	}
	var out []model.FlagRow
	for _, loc := range []flagLoc{{key, 0, 0}, {key, t, 0}, {key, t, f}} {
		if r, ok := m.rows[loc]; ok {
			out = append(out, model.FlagRow{ScopeType: r.st, Value: r.value})
		}
	}
	return out, nil
}

func (m *memFlagStore) LockTx(_ context.Context, _ *repository.DB, key model.FlagKey, t, f uint64) (repository.FlagCurrent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[flagLoc{key, t, f}]
	return repository.FlagCurrent{Value: r.value, RowVer: r.rowVer}, ok, nil
}

func (m *memFlagStore) UpsertTx(_ context.Context, _ *repository.DB, w model.FlagWrite) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeFail != nil {
		return m.writeFail
	}
	loc := flagLoc{w.Key, w.TenantID, w.FarmID}
	r, ok := m.rows[loc]
	if ok {
		r.rowVer++
	}
	r.st, r.value = w.ScopeType, w.Value
	m.rows[loc] = r
	return nil
}

func (m *memFlagStore) UpdateTx(_ context.Context, _ *repository.DB, w model.FlagWrite, expect int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	loc := flagLoc{w.Key, w.TenantID, w.FarmID}
	r, ok := m.rows[loc]
	if !ok || r.rowVer != expect {
		return false, nil
	}
	r.value, r.rowVer = w.Value, r.rowVer+1
	m.rows[loc] = r
	return true, nil
}

func (m *memFlagStore) List(_ context.Context, key model.FlagKey) ([]model.FlagSetting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.FlagSetting
	for loc, r := range m.rows {
		if loc.key == key {
			out = append(out, model.FlagSetting{TenantID: loc.tenantID, FarmID: loc.farmID, ScopeType: r.st, Value: r.value,
				RowVer: r.rowVer})
		}
	}
	return out, nil
}

func (m *memFlagStore) ScopeExists(_ context.Context, t, f uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.farms {
		if k[0] == t && (f == 0 || k[1] == f) {
			return true, nil
		}
	}
	return false, nil
}

func (m *memFlagStore) AppendTx(_ context.Context, _ *repository.DB, r model.AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auditFail != nil {
		return m.auditFail
	}
	m.audits = append(m.audits, r)
	return nil
}

func (m *memFlagStore) readCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

// memPub 记录 PUBLISH。
type memPub struct {
	mu   sync.Mutex
	msgs []string
	err  error
}

func (p *memPub) Publish(_ context.Context, channel string, msg []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, channel+" "+string(msg))
	return p.err
}

// memRoles 是 FarmRoleChecker 的桩：has 中的 (tenant, farm, role) 视为有绑定。
type memRoles struct {
	has map[string]bool
	err error
}

func (r memRoles) FarmHasRole(_ context.Context, t, f uint64, role model.Role) (bool, error) {
	return r.has[fmt.Sprintf("%d/%d/%s", t, f, role)], r.err
}

// stepClock 是可推进的时钟。
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type flagFixture struct {
	store *memFlagStore
	pub   *memPub
	clock *stepClock
	svc   *FlagService
}

// newFlagFixture：农场 (1,1) 有 agronomist，(1,2) 没有；enable_advice 在 (1,1)、(1,2) 开。
func newFlagFixture(t *testing.T, mutate func(*FlagDeps)) *flagFixture {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &flagFixture{store: newMemFlagStore([2]uint64{1, 1}, [2]uint64{1, 2}, [2]uint64{1, 3}), pub: &memPub{},
		clock: &stepClock{t: flagNow}}
	f.store.put(model.FlagEnableAdvice, 1, 1, `{"on":true}`)
	f.store.put(model.FlagEnableAdvice, 1, 2, `{"on":true}`)
	d := FlagDeps{Store: f.store, Tx: f.store, Audit: f.store, Pub: f.pub, Keys: keys, Clock: f.clock,
		FarmRoles: memRoles{has: map[string]bool{"1/1/agronomist": true}},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mutate != nil {
		mutate(&d)
	}
	if f.svc, err = NewFlagService(d); err != nil {
		t.Fatal(err)
	}
	return f
}

var flagAdmin = model.Scope{UserID: 9001, Roles: []model.Role{model.RolePlatformAdmin}}

func intPtr(n int) *int { return &n }
