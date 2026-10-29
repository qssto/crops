package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// memSub 是 repository.Subscription 的内存实现。
type memSub struct {
	ch     chan []byte
	once   sync.Once
	closed chan struct{}
}

func newMemSub() *memSub { return &memSub{ch: make(chan []byte), closed: make(chan struct{})} }

func (s *memSub) Messages() <-chan []byte { return s.ch }
func (s *memSub) Close() error            { s.once.Do(func() { close(s.closed) }); return nil }

type memSubscriber struct {
	sub     *memSub
	channel string
	err     error
}

func (m *memSubscriber) Subscribe(_ context.Context, channel string) (repository.Subscription, error) {
	m.channel = channel
	if m.err != nil {
		return nil, m.err
	}
	return m.sub, nil
}

// memBackfill 按 visible 截断返回 run_event（模拟事件逐步落库），并记录每次补读的 after。
type memBackfill struct {
	mu      sync.Mutex
	events  []repository.SSEMessage
	visible int64
	afters  []int64
	err     error
}

func (b *memBackfill) EventsAfter(_ context.Context, _ model.Scope, _ uint64, after int64) ([]repository.SSEMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.afters = append(b.afters, after)
	var out []repository.SSEMessage
	for _, e := range b.events {
		if e.Seq > after && e.Seq <= b.visible {
			out = append(out, e)
		}
	}
	return out, b.err
}

func (b *memBackfill) show(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.visible = n
}

type memRuns struct {
	status model.RunStatus
	err    error
}

func (r memRuns) Summary(context.Context, model.Scope, uint64) (model.RunSummary, error) {
	return model.RunSummary{RunID: 5, Status: r.status}, r.err
}

// recordSink 记录推送序列：seq 数字、"ping"、"end"。
type recordSink struct {
	mu     sync.Mutex
	frames []string
	events []StreamEvent
	pinged chan struct{}
}

func (s *recordSink) Event(e StreamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames, s.events = append(s.frames, fmt.Sprint(e.Seq)), append(s.events, e)
	return nil
}

func (s *recordSink) Ping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, "ping")
	if s.pinged != nil {
		select {
		case s.pinged <- struct{}{}:
		default:
		}
	}
	return nil
}

func (s *recordSink) End() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, "end")
	return nil
}

func (s *recordSink) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.frames, ",")
}

func sseMsg(seq int64, typ model.EventType) repository.SSEMessage {
	return repository.SSEMessage{RunID: 5, Seq: seq, EventType: typ, Actor: model.ActorAgent,
		Payload: json.RawMessage(`{}`), CreatedAt: dispatchNow}
}

func sseRaw(t *testing.T, m repository.SSEMessage) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type streamFixture struct {
	sub   *memSubscriber
	back  *memBackfill
	sink  *recordSink
	svc   *EventService
	keys  rediskey.Keys
	scope model.Scope
}

func newStreamFixture(t *testing.T, status model.RunStatus, events []repository.SSEMessage, visible int64) *streamFixture {
	t.Helper()
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	f := &streamFixture{sub: &memSubscriber{sub: newMemSub()}, back: &memBackfill{events: events, visible: visible},
		sink: &recordSink{}, keys: keys, scope: model.Scope{TenantID: 1, FarmID: 1, UserID: 1002}}
	f.svc = NewEventService(EventDeps{Subscriber: f.sub, Backfill: f.back, Runs: memRuns{status: status}, Keys: keys,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return f
}

// start 在后台推送，返回结果 channel。
func (f *streamFixture) start(ctx context.Context, lastSeq int64) <-chan error {
	out := make(chan error, 1)
	go func() { out <- f.svc.Stream(ctx, f.scope, 5, lastSeq, f.sink) }()
	return out
}

func waitStream(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return")
		return nil
	}
}

func (f *streamFixture) send(t *testing.T, raw []byte) {
	t.Helper()
	select {
	case f.sub.sub.ch <- raw:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not reading subscription")
	}
}

// 续推：先按 Last-Event-ID 补读，再推实时消息；已推送 / 别的 run / 坏消息丢弃；seq 断档补读；RUN_COMPLETED 后 end。
func TestStream_BackfillDedupeGap(t *testing.T) {
	events := []repository.SSEMessage{sseMsg(1, model.EventRunStarted), sseMsg(2, model.EventStepStarted),
		sseMsg(3, model.EventStepEnded), sseMsg(4, model.EventStepStarted), sseMsg(5, model.EventStepEnded),
		sseMsg(6, model.EventStepStarted), sseMsg(7, model.EventRunCompleted)}
	events[1].StepID = "plan"
	f := newStreamFixture(t, model.RunRunning, events, 3)
	done := f.start(context.Background(), 1)
	f.send(t, sseRaw(t, events[1])) // 已补读：丢弃
	other := events[3]
	other.RunID = 6
	f.send(t, sseRaw(t, other)) // 别的 run：丢弃
	f.send(t, []byte("{bad"))   // 坏消息：丢弃
	f.send(t, sseRaw(t, events[3]))
	f.back.show(6)
	f.send(t, sseRaw(t, events[5])) // seq 6，断档：补读 5、6
	f.send(t, sseRaw(t, events[6]))
	if err := waitStream(t, done); err != nil {
		t.Fatal(err)
	}
	if got := f.sink.got(); got != "2,3,4,5,6,7,end" {
		t.Errorf("frames = %s", got)
	}
	f.back.mu.Lock()
	afters := fmt.Sprint(f.back.afters)
	f.back.mu.Unlock()
	if afters != "[1 4]" {
		t.Errorf("backfill afters = %s, want [1 4]", afters)
	}
	if f.sub.channel != f.keys.SSERun(5) {
		t.Errorf("subscribed %q", f.sub.channel)
	}
	if e := f.sink.events[0]; e.StepID == nil || *e.StepID != "plan" || e.ActorType != model.ActorAgent ||
		f.sink.events[1].StepID != nil {
		t.Errorf("first events = %+v", f.sink.events[:2])
	}
	select {
	case <-f.sub.sub.closed:
	default:
		t.Error("subscription not closed")
	}
}

// 补读即结束：run 已终态 / 中断时补读完发 end；补读到结束事件时在其后 end、不再看 run 状态。
func TestStream_EndsAfterBackfill(t *testing.T) {
	tests := []struct {
		name    string
		status  model.RunStatus
		events  []repository.SSEMessage
		lastSeq int64
		want    string
	}{
		{name: "completed run", status: model.RunCompleted,
			events: []repository.SSEMessage{sseMsg(1, model.EventRunStarted), sseMsg(2, model.EventStepEnded)}, want: "1,2,end"},
		{name: "interrupted run", status: model.RunInterrupted,
			events: []repository.SSEMessage{sseMsg(1, model.EventRunStarted)}, want: "1,end"},
		{name: "interrupted event", status: model.RunRunning,
			events: []repository.SSEMessage{sseMsg(1, model.EventRunStarted), sseMsg(2, model.EventRunInterrupted)}, want: "1,2,end"},
		{name: "failed event resumed after it", status: model.RunFailed,
			events: []repository.SSEMessage{sseMsg(1, model.EventRunStarted), sseMsg(2, model.EventRunFailed)}, lastSeq: 2,
			want: "end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newStreamFixture(t, tt.status, tt.events, 100)
			if err := waitStream(t, f.start(context.Background(), tt.lastSeq)); err != nil {
				t.Fatal(err)
			}
			if got := f.sink.got(); got != tt.want {
				t.Errorf("frames = %s, want %s", got, tt.want)
			}
		})
	}
}

// 失败路径：游标为负、订阅失败、run 不存在（不写 sink）、补读失败、订阅断开。
func TestStream_Errors(t *testing.T) {
	notFound := newStreamFixture(t, "", nil, 0)
	notFound.svc.d.Runs = memRuns{err: model.ErrNotFound}
	if err := notFound.svc.Stream(context.Background(), notFound.scope, 5, 0, notFound.sink); !errors.Is(err, model.ErrNotFound) ||
		notFound.sink.got() != "" {
		t.Errorf("not found: err = %v frames = %q", err, notFound.sink.got())
	}
	neg := newStreamFixture(t, model.RunRunning, nil, 0)
	if err := neg.svc.Stream(context.Background(), neg.scope, 5, -1, neg.sink); !errors.Is(err, ErrBadInput) || neg.sub.channel != "" {
		t.Errorf("negative cursor: err = %v subscribed %q", err, neg.sub.channel)
	}
	subFail := newStreamFixture(t, model.RunRunning, nil, 0)
	subFail.sub.err = errors.New("redis down")
	if err := subFail.svc.Stream(context.Background(), subFail.scope, 5, 0, subFail.sink); err == nil {
		t.Error("subscribe failure swallowed")
	}
	backFail := newStreamFixture(t, model.RunRunning, nil, 0)
	backFail.back.err = errors.New("db down")
	if err := backFail.svc.Stream(context.Background(), backFail.scope, 5, 0, backFail.sink); err == nil ||
		!strings.Contains(err.Error(), "db down") {
		t.Errorf("backfill failure: err = %v", err)
	}
	closed := newStreamFixture(t, model.RunRunning, nil, 0)
	done := closed.start(context.Background(), 0)
	close(closed.sub.sub.ch)
	if err := waitStream(t, done); !errors.Is(err, ErrSubscriptionClosed) {
		t.Errorf("closed subscription: err = %v", err)
	}
}

// 保活与退出：周期 ping；ctx 取消或 Shutdown 时返回 nil 且不发 end。
func TestStream_PingCancelShutdown(t *testing.T) {
	f := newStreamFixture(t, model.RunRunning, nil, 0)
	f.svc.d.Ping = time.Millisecond
	f.sink.pinged = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := f.start(ctx, 0)
	select {
	case <-f.sink.pinged:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping")
	}
	cancel()
	if err := waitStream(t, done); err != nil {
		t.Errorf("cancel err = %v", err)
	}
	if strings.Contains(f.sink.got(), "end") {
		t.Errorf("frames after cancel = %s", f.sink.got())
	}
	g := newStreamFixture(t, model.RunRunning, nil, 0)
	done = g.start(context.Background(), 0)
	g.svc.Shutdown()
	g.svc.Shutdown() // 可重复调用
	if err := waitStream(t, done); err != nil || g.sink.got() != "" {
		t.Errorf("shutdown err = %v frames = %q", err, g.sink.got())
	}
}

func TestNewEventService_DefaultPing(t *testing.T) {
	if s := NewEventService(EventDeps{}); s.d.Ping != DefaultSSEPing || DefaultSSEPing != 15*time.Second {
		t.Errorf("ping = %s", s.d.Ping)
	}
}
