package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/repository"
)

var outboxNow = time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)

// memOutbox 是 OutboxStore 的内存 fake：记录领取参数、DONE 与退避。
type memOutbox struct {
	mu         sync.Mutex
	msgs       []repository.OutboxMessage
	claimErr   error
	retryErr   error
	retryOK    bool
	claims     int
	leaseUntil time.Time
	done       []uint64
	retries    []outboxRetry
}

type outboxRetry struct {
	id      uint64
	next    time.Time
	lastErr string
}

func (m *memOutbox) Claim(_ context.Context, _, leaseUntil time.Time) ([]repository.OutboxMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claims++
	m.leaseUntil = leaseUntil
	if m.claimErr != nil {
		return nil, m.claimErr
	}
	out := m.msgs
	m.msgs = nil
	return out, nil
}

func (m *memOutbox) MarkDone(_ context.Context, id uint64, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done = append(m.done, id)
	return true, nil
}

func (m *memOutbox) MarkRetry(_ context.Context, id uint64, next time.Time, lastErr string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retryErr != nil {
		return false, m.retryErr
	}
	m.retries = append(m.retries, outboxRetry{id: id, next: next, lastErr: lastErr})
	return m.retryOK, nil
}

func newPoller(store *memOutbox, h OutboxHandler) (*OutboxPoller, *bytes.Buffer) {
	var buf bytes.Buffer
	p := NewOutboxPoller(OutboxDeps{Store: store, Handlers: map[string]OutboxHandler{repository.TopicOrderDispatch: h},
		Clock: fixedClock{outboxNow}, Logger: slog.New(slog.NewTextHandler(&buf, nil)), Interval: time.Millisecond})
	return p, &buf
}

func TestOutboxRetryDelay(t *testing.T) {
	want := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for attempts, w := range want {
		if got := outboxRetryDelay(attempts); got != w {
			t.Errorf("outboxRetryDelay(%d) = %s, want %s", attempts, got, w)
		}
	}
	if got := outboxRetryDelay(-1); got != time.Second {
		t.Errorf("outboxRetryDelay(-1) = %s", got)
	}
}

// 一轮处理：成功置 DONE；失败按已失败次数退避；第 5 次失败 ERROR 告警 DEAD；未知 topic / panic 走退避；行已非 PENDING 只告警。
func TestOutboxRunOnce(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		msg       repository.OutboxMessage
		handler   OutboxHandler
		retryOK   bool
		wantDone  bool
		wantNext  time.Duration // 有退避时的 next_run_at - now
		wantLog   string        // 日志须含
		wantInErr string        // lastErr 须含
	}{
		{name: "success", msg: repository.OutboxMessage{ID: 1, Topic: repository.TopicOrderDispatch},
			handler: func(context.Context, repository.OutboxMessage) error { return nil }, wantDone: true},
		{name: "first failure backs off 1s", msg: repository.OutboxMessage{ID: 2, Topic: repository.TopicOrderDispatch},
			handler: func(context.Context, repository.OutboxMessage) error { return boom }, retryOK: true,
			wantNext: time.Second, wantLog: `level=WARN msg="outbox message retry"`, wantInErr: "boom"},
		{name: "third failure backs off 30s", msg: repository.OutboxMessage{ID: 3, Topic: repository.TopicOrderDispatch, Attempts: 2},
			handler: func(context.Context, repository.OutboxMessage) error { return boom }, retryOK: true,
			wantNext: 30 * time.Second, wantLog: "outbox message retry"},
		{name: "fifth failure is dead", msg: repository.OutboxMessage{ID: 4, Topic: repository.TopicOrderDispatch, Attempts: 4},
			handler: func(context.Context, repository.OutboxMessage) error { return boom }, retryOK: true,
			wantNext: 10 * time.Minute, wantLog: `level=ERROR msg="outbox message dead"`},
		{name: "unknown topic retries", msg: repository.OutboxMessage{ID: 5, Topic: "nope"}, retryOK: true,
			wantNext: time.Second, wantInErr: ErrUnknownTopic.Error()},
		{name: "panic recovered", msg: repository.OutboxMessage{ID: 6, Topic: repository.TopicOrderDispatch},
			handler: func(context.Context, repository.OutboxMessage) error { panic("kaboom") }, retryOK: true,
			wantNext: time.Second, wantInErr: "kaboom"},
		{name: "row no longer pending", msg: repository.OutboxMessage{ID: 7, Topic: repository.TopicOrderDispatch},
			handler: func(context.Context, repository.OutboxMessage) error { return boom }, retryOK: false,
			wantNext: time.Second, wantLog: "no longer pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &memOutbox{msgs: []repository.OutboxMessage{tt.msg}, retryOK: tt.retryOK}
			p, logs := newPoller(store, tt.handler)
			n, err := p.RunOnce(context.Background())
			if err != nil || n != 1 {
				t.Fatalf("RunOnce = %d, %v", n, err)
			}
			if !store.leaseUntil.Equal(outboxNow.Add(DefaultOutboxLease)) {
				t.Errorf("lease until %s", store.leaseUntil)
			}
			if tt.wantDone {
				if len(store.done) != 1 || len(store.retries) != 0 {
					t.Errorf("done=%v retries=%v", store.done, store.retries)
				}
				return
			}
			if len(store.done) != 0 || len(store.retries) != 1 {
				t.Fatalf("done=%v retries=%v", store.done, store.retries)
			}
			r := store.retries[0]
			if r.id != tt.msg.ID || r.next.Sub(outboxNow) != tt.wantNext || !strings.Contains(r.lastErr, tt.wantInErr) {
				t.Errorf("retry = %+v, want next +%s lastErr ~ %q", r, tt.wantNext, tt.wantInErr)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("logs = %s, want %q", logs, tt.wantLog)
			}
		})
	}
}

// 领取失败与退避记账失败都向上返回（不吞错）；ctx 已取消时不开始处理。
func TestOutboxRunOnce_Errors(t *testing.T) {
	claimFail := &memOutbox{claimErr: errors.New("db down")}
	p, _ := newPoller(claimFail, nil)
	if _, err := p.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("claim err = %v", err)
	}
	retryFail := &memOutbox{msgs: []repository.OutboxMessage{{ID: 1, Topic: repository.TopicOrderDispatch}},
		retryErr: errors.New("write failed")}
	p, _ = newPoller(retryFail, func(context.Context, repository.OutboxMessage) error { return errors.New("boom") })
	if _, err := p.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "write failed") ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("retry err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	canceled := &memOutbox{msgs: []repository.OutboxMessage{{ID: 1, Topic: repository.TopicOrderDispatch}}}
	p, _ = newPoller(canceled, func(context.Context, repository.OutboxMessage) error { called = true; return nil })
	if n, err := p.RunOnce(ctx); err != nil || n != 0 || called {
		t.Errorf("canceled RunOnce = %d, %v, handler called %t", n, err, called)
	}
}

// Run 周期处理直到 ctx 取消，返回后不再领取。
func TestOutboxRun_StopsOnCancel(t *testing.T) {
	handled := make(chan uint64, 1)
	store := &memOutbox{msgs: []repository.OutboxMessage{{ID: 9, Topic: repository.TopicOrderDispatch}}}
	p, _ := newPoller(store, func(_ context.Context, m repository.OutboxMessage) error { handled <- m.ID; return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	select {
	case id := <-handled:
		if id != 9 {
			t.Errorf("handled %d", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("message not handled")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	store.mu.Lock()
	claims := store.claims
	store.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != claims {
		t.Errorf("claims after stop: %d -> %d", claims, store.claims)
	}
}
