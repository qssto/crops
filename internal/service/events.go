package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// DefaultSSEPing 是 SSE 保活注释的周期（L8 冻结契约：每 15s 一条 `: ping`）。
const DefaultSSEPing = 15 * time.Second

// ErrSubscriptionClosed 表示 Redis 订阅在推送中被关闭（连接断开），客户端按 Last-Event-ID 重连续推。
var ErrSubscriptionClosed = errors.New("service: sse subscription closed")

// EventSubscriber 订阅 Redis 频道，repository.RedisSubscriber 实现；返回时订阅已生效。
type EventSubscriber interface {
	Subscribe(ctx context.Context, channel string) (repository.Subscription, error)
}

// EventBackfill 按 seq 补读 run_event，repository.EventBackfillRepo 实现。
type EventBackfill interface {
	EventsAfter(ctx context.Context, scope model.Scope, runID uint64, after int64) ([]repository.SSEMessage, error)
}

// RunSummaryReader 读 run 状态（限定租户 / 农场，不存在返回 model.ErrNotFound），repository.RunRepo 实现。
type RunSummaryReader interface {
	Summary(ctx context.Context, scope model.Scope, runID uint64) (model.RunSummary, error)
}

// EventDeps 是 EventService 的依赖。
type EventDeps struct {
	Subscriber EventSubscriber
	Backfill   EventBackfill
	Runs       RunSummaryReader
	Keys       rediskey.Keys
	Logger     *slog.Logger
	Ping       time.Duration // 0 取 DefaultSSEPing
}

// StreamEvent 是推给客户端的一条事件（SSE data 的 JSON，L8 冻结契约）。
type StreamEvent struct {
	Seq        int64           `json:"seq"`
	EventType  model.EventType `json:"event_type"`
	StepID     *string         `json:"step_id"`
	ToolCallID *uint64         `json:"tool_call_id"`
	ActorType  model.ActorType `json:"actor_type"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// StreamSink 是 SSE 输出端（controller 实现，只负责写帧与 flush）。
type StreamSink interface {
	Event(e StreamEvent) error
	Ping() error
	End() error
}

// EventService 是 run 事件流（P0 §6.9 流程 ①2）。
type EventService struct {
	d    EventDeps
	stop chan struct{}
	once sync.Once
}

// NewEventService 构造 EventService。
func NewEventService(d EventDeps) *EventService {
	if d.Ping <= 0 {
		d.Ping = DefaultSSEPing
	}
	return &EventService{d: d, stop: make(chan struct{})}
}

// Shutdown 让进行中的推送立即返回（不发 end，客户端按 Last-Event-ID 重连到其他实例）；
// http.Server.Shutdown 不取消长连接的请求 ctx，须经 RegisterOnShutdown 调用本方法。可重复调用。
func (s *EventService) Shutdown() { s.once.Do(func() { close(s.stop) }) }

// Stream 推送 run 的事件直到 run 结束、ctx 取消或订阅断开：先 SUBSCRIBE（缓冲）→ 读 run（不存在返回 model.ErrNotFound，
// 此前不写 sink）→ 补读 seq > lastSeq → 推缓冲与后续消息，丢弃 seq ≤ 已推送的消息，seq 断档时再补读。
// 推出 RUN_COMPLETED / RUN_FAILED / RUN_INTERRUPTED，或补读后 run 已终态 / 中断，发 End 并返回 nil。
func (s *EventService) Stream(ctx context.Context, scope model.Scope, runID uint64, lastSeq int64, sink StreamSink) error {
	if lastSeq < 0 {
		return fmt.Errorf("%w: last_event_id must be >= 0", ErrBadInput)
	}
	sub, err := s.d.Subscriber.Subscribe(ctx, s.d.Keys.SSERun(runID))
	if err != nil {
		return fmt.Errorf("service: stream run %d: %w", runID, err)
	}
	defer func() {
		if err := sub.Close(); err != nil {
			s.d.Logger.WarnContext(ctx, "sse unsubscribe", "run_id", runID, "err", err)
		}
	}()
	sum, err := s.d.Runs.Summary(ctx, scope, runID)
	if err != nil {
		return fmt.Errorf("service: stream run %d: %w", runID, err)
	}
	st := &streamState{svc: s, scope: scope, runID: runID, last: lastSeq, sink: sink}
	ended, err := st.backfill(ctx)
	if err != nil || ended {
		return err
	}
	if sum.Status.IsTerminal() || sum.Status == model.RunInterrupted {
		return st.end()
	}
	ping := time.NewTicker(s.d.Ping)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.stop:
			return nil
		case <-ping.C:
			if err := sink.Ping(); err != nil {
				return fmt.Errorf("service: stream run %d ping: %w", runID, err)
			}
		case raw, ok := <-sub.Messages():
			if !ok {
				return fmt.Errorf("service: stream run %d: %w", runID, ErrSubscriptionClosed)
			}
			ended, err := st.live(ctx, raw)
			if err != nil || ended {
				return err
			}
		}
	}
}

// streamState 是一次推送的游标：last 为已推送的最大 seq。
type streamState struct {
	svc   *EventService
	scope model.Scope
	runID uint64
	last  int64
	sink  StreamSink
}

// live 处理一条订阅消息：别的 run / 已推送的丢弃；断档补读（补读结果已含本条）；否则直接推送。
func (st *streamState) live(ctx context.Context, raw []byte) (bool, error) {
	var m repository.SSEMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		st.svc.d.Logger.WarnContext(ctx, "sse bad message dropped", "run_id", st.runID, "err", err)
		return false, nil
	}
	switch {
	case m.RunID != st.runID, m.Seq <= st.last:
		return false, nil
	case m.Seq > st.last+1:
		return st.backfill(ctx)
	}
	return st.emit(m)
}

// backfill 补读 seq > last 并依次推送。
func (st *streamState) backfill(ctx context.Context) (bool, error) {
	msgs, err := st.svc.d.Backfill.EventsAfter(ctx, st.scope, st.runID, st.last)
	if err != nil {
		return false, fmt.Errorf("service: stream run %d backfill: %w", st.runID, err)
	}
	for _, m := range msgs {
		if m.Seq <= st.last {
			continue
		}
		if ended, err := st.emit(m); err != nil || ended {
			return ended, err
		}
	}
	return false, nil
}

// emit 推送一条并前移游标；结束类事件之后发 End。
func (st *streamState) emit(m repository.SSEMessage) (bool, error) {
	e := StreamEvent{Seq: m.Seq, EventType: m.EventType, ToolCallID: m.ToolCallID, ActorType: m.Actor,
		Payload: m.Payload, CreatedAt: m.CreatedAt}
	if m.StepID != "" {
		step := m.StepID
		e.StepID = &step
	}
	if err := st.sink.Event(e); err != nil {
		return false, fmt.Errorf("service: stream run %d seq %d: %w", st.runID, m.Seq, err)
	}
	st.last = m.Seq
	if endsStream(m.EventType) {
		return true, st.end()
	}
	return false, nil
}

func (st *streamState) end() error {
	if err := st.sink.End(); err != nil {
		return fmt.Errorf("service: stream run %d end: %w", st.runID, err)
	}
	return nil
}

// endsStream：run 终态或中断事件后结束推送（L8 冻结契约）。
func endsStream(t model.EventType) bool {
	return t == model.EventRunCompleted || t == model.EventRunFailed || t == model.EventRunInterrupted
}
