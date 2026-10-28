package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/model"
)

// 本文件是 gateway 单测用的内存假实现。

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// handler 处理一次 Invoke；n 是该工具第几次被调用（从 1 开始）。
type handler func(ctx context.Context, args json.RawMessage, n int) (mcp.Result, error)

// fakeServer 按工具名分发并计数。
type fakeServer struct {
	name     string
	specs    []mcp.ToolSpec
	handlers map[string]handler

	mu    sync.Mutex
	calls map[string]int
}

func (s *fakeServer) Name() string               { return s.name }
func (s *fakeServer) Tools() []mcp.ToolSpec      { return s.specs }
func (s *fakeServer) count(tool string) int      { s.mu.Lock(); defer s.mu.Unlock(); return s.calls[tool] }
func (s *fakeServer) handle(t string, h handler) { s.handlers[t] = h }

func (s *fakeServer) Invoke(ctx context.Context, tool string, args json.RawMessage) (mcp.Result, error) {
	s.mu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[tool]++
	n := s.calls[tool]
	h := s.handlers[tool]
	s.mu.Unlock()
	if h == nil {
		return mcp.Result{}, errors.New("no handler")
	}
	return h(ctx, args, n)
}

// memIdem 是 IdemStore 的内存实现（不模拟过期）。
type memIdem struct {
	mu   sync.Mutex
	m    map[string]string
	fail bool
}

func newMemIdem() *memIdem { return &memIdem{m: map[string]string{}} }

var errRedisDown = errors.New("redis down")

func (r *memIdem) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return false, errRedisDown
	}
	if _, ok := r.m[key]; ok {
		return false, nil
	}
	r.m[key] = value
	return true, nil
}

func (r *memIdem) Get(_ context.Context, key string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return "", false, errRedisDown
	}
	v, ok := r.m[key]
	return v, ok, nil
}

func (r *memIdem) Replace(_ context.Context, key, expect, value string, _ time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return false, errRedisDown
	}
	if r.m[key] != expect {
		return false, nil
	}
	r.m[key] = value
	return true, nil
}

func (r *memIdem) DeleteIf(_ context.Context, key, expect string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return false, errRedisDown
	}
	if v, ok := r.m[key]; !ok || v != expect {
		return false, nil
	}
	delete(r.m, key)
	return true, nil
}

func (r *memIdem) value(key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.m[key]
	return v, ok
}

func (r *memIdem) set(key, v string) { r.mu.Lock(); defer r.mu.Unlock(); r.m[key] = v }
func (r *memIdem) clear()            { r.mu.Lock(); defer r.mu.Unlock(); r.m = map[string]string{} }

// memCache 是 Cache 的内存实现，记录写入的 TTL。
type memCache struct {
	mu  sync.Mutex
	m   map[string][]byte
	ttl map[string]time.Duration
}

func newMemCache() *memCache {
	return &memCache{m: map[string][]byte{}, ttl: map[string]time.Duration{}}
}

func (c *memCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[key]
	return b, ok, nil
}

func (c *memCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
	c.ttl[key] = ttl
	return nil
}

func (c *memCache) size() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.m) }

// memCalls 是 ToolCallStore 的内存实现：attempt 按 call_key 递增。
type memCalls struct {
	mu   sync.Mutex
	rows []savedRow
}

type savedRow struct {
	id      uint64
	attempt int
	row     model.ToolCallRow
}

func (s *memCalls) Save(_ context.Context, row model.ToolCallRow) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := 1
	for _, r := range s.rows {
		if r.row.TenantID == row.TenantID && r.row.FarmID == row.FarmID && r.row.CallKey == row.CallKey && r.attempt >= attempt {
			attempt = r.attempt + 1
		}
	}
	id := uint64(len(s.rows) + 1)
	s.rows = append(s.rows, savedRow{id: id, attempt: attempt, row: row})
	return id, nil
}

func (s *memCalls) FindOK(_ context.Context, scope model.Scope, callKey string) (model.ToolCallHit, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[i]
		if r.row.TenantID == scope.TenantID && r.row.FarmID == scope.FarmID && r.row.CallKey == callKey &&
			r.row.Status == model.ToolStatusOK {
			return model.ToolCallHit{ID: r.id, ResultSummary: r.row.ResultSummary, RawObjectID: r.row.RawObjectID}, true, nil
		}
	}
	return model.ToolCallHit{}, false, nil
}

func (s *memCalls) all() []savedRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]savedRow(nil), s.rows...)
}

func (s *memCalls) last() savedRow {
	rows := s.all()
	return rows[len(rows)-1]
}

// memRaw 是 RawStore 的内存实现。
type memRaw struct {
	mu    sync.Mutex
	fail  bool
	names []string
}

func (r *memRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return model.RawObject{}, errors.New("minio down")
	}
	r.names = append(r.names, p.Name)
	return model.RawObject{ID: uint64(100 + len(r.names)), Kind: p.Kind}, nil
}

// memFields 是 FieldScope 的内存实现：field_id → (tenant, farm)。
type memFields struct {
	owner map[uint64][2]uint64
	err   error
}

func (f memFields) Owns(_ context.Context, s model.Scope, fieldID uint64) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	o, ok := f.owner[fieldID]
	return ok && o[0] == s.TenantID && o[1] == s.FarmID, nil
}

// memEvents 记录发出的事件。
type memEvents struct {
	mu  sync.Mutex
	evs []model.RunEvent
}

func (e *memEvents) Emit(_ context.Context, ev model.RunEvent) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evs = append(e.evs, ev)
	return int64(len(e.evs)), nil
}
