package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

var outboxNow = time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)

// memOutbox 是 OutboxStore 的内存 fake：记录领取参数、续租、DONE、推后与退避，按 row_ver 校验租约。
type memOutbox struct {
	mu         sync.Mutex
	msgs       []repository.OutboxMessage
	claimErr   error
	retryErr   error
	retryOK    bool
	renewLost  bool // 续租影响 0 行（租约被他人接走）
	claims     int
	leaseUntil time.Time
	renews     []int // 续租时带的 row_ver
	done       []uint64
	doneVers   []int
	defers     []outboxRetry
	retries    []outboxRetry
}

type outboxRetry struct {
	id      uint64
	rowVer  int
	next    time.Time
	lastErr string
}

func (m *memOutbox) Claim(_ context.Context, _ repository.SystemScope, _, leaseUntil time.Time) ([]repository.OutboxMessage, error) {
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

func (m *memOutbox) Renew(_ context.Context, _ repository.SystemScope, _ uint64, rowVer int, _, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.renews = append(m.renews, rowVer)
	return !m.renewLost, nil
}

func (m *memOutbox) renewCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.renews)
}

func (m *memOutbox) MarkDone(_ context.Context, _ repository.SystemScope, id uint64, rowVer int, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done = append(m.done, id)
	m.doneVers = append(m.doneVers, rowVer)
	return true, nil
}

func (m *memOutbox) Defer(_ context.Context, _ repository.SystemScope, id uint64, rowVer int, next, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defers = append(m.defers, outboxRetry{id: id, rowVer: rowVer, next: next})
	return true, nil
}

func (m *memOutbox) MarkRetry(_ context.Context, _ repository.SystemScope, id uint64, rowVer int, next time.Time, lastErr string,
	_ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.retryErr != nil {
		return false, m.retryErr
	}
	m.retries = append(m.retries, outboxRetry{id: id, rowVer: rowVer, next: next, lastErr: lastErr})
	return m.retryOK, nil
}

func newPoller(store *memOutbox, h OutboxHandler) (*OutboxPoller, *bytes.Buffer) {
	return newPollerWith(store, h, nil, 0)
}

// newPollerWith 可注入 DEAD 审计与续租周期（0 取默认 20s，单测里等同不续租）。
func newPollerWith(store *memOutbox, h OutboxHandler, audit AuditAppender, renew time.Duration) (*OutboxPoller, *bytes.Buffer) {
	var buf bytes.Buffer
	p := NewOutboxPoller(OutboxDeps{Store: store, Handlers: map[string]OutboxHandler{repository.TopicOrderDispatch: h},
		Audit: audit, Clock: fixedClock{outboxNow}, Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Interval: time.Millisecond, Renew: renew})
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

// 续租影响 0 行：处理器 ctx 被取消（cause ErrOutboxLeaseLost），处理返回后不写 DONE / 推后 / 退避（应失败：写了任何结果）。
func TestOutboxHandle_LeaseLostWritesNothing(t *testing.T) {
	for _, result := range []string{"handler succeeds anyway", "handler fails"} {
		t.Run(result, func(t *testing.T) {
			store := &memOutbox{msgs: []repository.OutboxMessage{{ID: 1, Topic: repository.TopicOrderDispatch, RowVer: 3}},
				renewLost: true, retryOK: true}
			var cause error
			p, logs := newPollerWith(store, func(ctx context.Context, _ repository.OutboxMessage) error {
				<-ctx.Done()
				cause = context.Cause(ctx)
				if result == "handler fails" {
					return ctx.Err()
				}
				return nil
			}, nil, time.Millisecond)
			if _, err := p.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if !errors.Is(cause, ErrOutboxLeaseLost) {
				t.Errorf("handler ctx cause = %v, want ErrOutboxLeaseLost", cause)
			}
			if len(store.done)+len(store.defers)+len(store.retries) != 0 {
				t.Errorf("lease lost but wrote done=%v defers=%v retries=%v", store.done, store.defers, store.retries)
			}
			if store.renews[0] != 3 || !strings.Contains(logs.String(), "outbox lease lost, result dropped") {
				t.Errorf("renews=%v logs=%s", store.renews, logs)
			}
		})
	}
}

// 处理中按周期续租，每次成功 row_ver+1；DONE 带最新 row_ver（应失败：带认领时的旧 row_ver 会影响 0 行）。
func TestOutboxHandle_RenewAdvancesToken(t *testing.T) {
	store := &memOutbox{msgs: []repository.OutboxMessage{{ID: 1, Topic: repository.TopicOrderDispatch, RowVer: 7}}}
	p, _ := newPollerWith(store, func(context.Context, repository.OutboxMessage) error {
		deadline := time.Now().Add(5 * time.Second)
		for store.renewCount() < 2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return nil
	}, nil, time.Millisecond)
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	n := len(store.renews)
	if n < 2 || store.renews[0] != 7 || store.renews[1] != 8 {
		t.Fatalf("renews = %v", store.renews)
	}
	if len(store.doneVers) != 1 || store.doneVers[0] != 7+n {
		t.Errorf("done row_ver = %v, want %d", store.doneVers, 7+n)
	}
}

// 拿不到 lock:order / 处理器要求推后：只推后 5s、不计 attempts（应失败：走了 MarkRetry）。
func TestOutboxHandle_DeferOnBusy(t *testing.T) {
	for name, herr := range map[string]error{"order busy": fmt.Errorf("x: %w", ErrOrderBusy),
		"deferred": fmt.Errorf("device offline: %w", ErrOutboxDeferred)} {
		t.Run(name, func(t *testing.T) {
			store := &memOutbox{msgs: []repository.OutboxMessage{{ID: 4, Topic: repository.TopicOrderDispatch, RowVer: 2,
				Attempts: 4}}, retryOK: true}
			p, _ := newPoller(store, func(context.Context, repository.OutboxMessage) error { return herr })
			if _, err := p.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(store.retries) != 0 || len(store.done) != 0 || len(store.defers) != 1 {
				t.Fatalf("retries=%v done=%v defers=%v", store.retries, store.done, store.defers)
			}
			if d := store.defers[0]; d.rowVer != 2 || d.next.Sub(outboxNow) != outboxDeferDelay {
				t.Errorf("defer = %+v", d)
			}
		})
	}
}

// 第 5 次失败转 DEAD：ERROR 日志 + OUTBOX_DEAD 审计（消息所属农场）；第 4 次失败不写审计。
func TestOutboxHandle_DeadAudit(t *testing.T) {
	boom := errors.New("boom")
	for _, attempts := range []int{3, 4} {
		audit := &memDenialStore{}
		store := &memOutbox{msgs: []repository.OutboxMessage{{ID: 5, TenantID: 1, FarmID: 2, Topic: repository.TopicOrderDispatch,
			DedupKey: "order.dispatch:9:3:1", Attempts: attempts}}, retryOK: true}
		p, _ := newPollerWith(store, func(context.Context, repository.OutboxMessage) error { return boom }, audit, 0)
		if _, err := p.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if attempts == 3 {
			if len(audit.recs) != 0 {
				t.Errorf("attempts 3 audited: %+v", audit.recs)
			}
			continue
		}
		if len(audit.recs) != 1 {
			t.Fatalf("dead audit recs = %d", len(audit.recs))
		}
		r := audit.recs[0]
		if r.Action != string(model.AuditOutboxDead) || r.TenantID != 1 || r.FarmID != 2 || r.Result != string(model.AuditResultFailed) ||
			!strings.Contains(string(r.Summary), "order.dispatch:9:3:1") {
			t.Errorf("dead audit = %+v summary %s", r, r.Summary)
		}
	}
}
