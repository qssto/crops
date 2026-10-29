package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/model"
)

// ssePublishTimeout 是单条 SSE 发布的超时（提交后执行，不受调用方 ctx 取消影响）。
const ssePublishTimeout = 2 * time.Second

// Publisher 是 SSE 发布能力（Redis PUBLISH）。
type Publisher interface {
	Publish(ctx context.Context, channel string, msg []byte) error
}

// RedisPublisher 用 go-redis 实现 Publisher。
type RedisPublisher struct{ Client redis.UniversalClient }

// Publish 实现 Publisher。
func (p RedisPublisher) Publish(ctx context.Context, channel string, msg []byte) error {
	if err := p.Client.Publish(ctx, channel, msg).Err(); err != nil {
		return fmt.Errorf("repository: publish %s: %w", channel, err)
	}
	return nil
}

// SSEMessage 是 PUBLISH sse:run:{run_id} 的消息体（订阅端按 seq 去重 / 补拉）。
type SSEMessage struct {
	RunID      uint64          `json:"run_id"`
	Seq        int64           `json:"seq"`
	EventType  model.EventType `json:"event_type"`
	StepID     string          `json:"step_id,omitempty"`
	ToolCallID *uint64         `json:"tool_call_id,omitempty"`
	Actor      model.ActorType `json:"actor"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// publish 发布一条事件；失败只告警（run_event 已落库，订阅端可按 seq 补拉）。
func (r EventRepo) publish(ctx context.Context, e model.RunEvent, seq int64) {
	body, err := json.Marshal(SSEMessage{RunID: e.RunID, Seq: seq, EventType: e.Type, StepID: e.StepID,
		ToolCallID: e.ToolCallID, Actor: e.Actor, Payload: e.Payload, CreatedAt: e.CreatedAt})
	if err != nil {
		r.warn(ctx, "sse encode failed", e, seq, err)
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ssePublishTimeout)
	defer cancel()
	if err := r.Pub.Publish(pctx, r.Keys.SSERun(e.RunID), body); err != nil {
		r.warn(ctx, "sse publish failed", e, seq, err)
	}
}

func (r EventRepo) warn(ctx context.Context, msg string, e model.RunEvent, seq int64, err error) {
	if r.Log == nil {
		return
	}
	attrs := []any{"run_id", e.RunID, "seq", seq, "event_type", string(e.Type)}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	r.Log.WarnContext(ctx, msg, attrs...)
}

// Subscription 是一个已确认的 Redis 频道订阅；Messages 在 Close 后关闭。
type Subscription interface {
	Messages() <-chan []byte
	Close() error
}

// RedisSubscriber 用 go-redis 实现 SSE 订阅（SUBSCRIBE sse:run:{run_id}）。
type RedisSubscriber struct{ Client redis.UniversalClient }

// sseSubscribeBuffer 是订阅缓冲；溢出时 go-redis 丢弃消息，订阅端靠 seq 断档补读兜底。
const sseSubscribeBuffer = 256

// Subscribe 订阅 channel，等到 Redis 确认订阅后返回（此后发布的消息不会落在补读与订阅之间的空隙里）。
func (s RedisSubscriber) Subscribe(ctx context.Context, channel string) (Subscription, error) {
	ps := s.Client.Subscribe(ctx, channel)
	if _, err := ps.Receive(ctx); err != nil {
		if cerr := ps.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, fmt.Errorf("repository: subscribe %s: %w", channel, err)
	}
	out := make(chan []byte, sseSubscribeBuffer)
	done := make(chan struct{})
	src := ps.Channel()
	go func() {
		defer close(out)
		for m := range src {
			select {
			case out <- []byte(m.Payload):
			case <-done:
				return
			}
		}
	}()
	return &redisSubscription{ps: ps, ch: out, done: done}, nil
}

type redisSubscription struct {
	ps   *redis.PubSub
	ch   chan []byte
	done chan struct{}
	once sync.Once
}

// Messages 实现 Subscription。
func (r *redisSubscription) Messages() <-chan []byte { return r.ch }

// Close 实现 Subscription（可重复调用）。
func (r *redisSubscription) Close() error {
	var err error
	r.once.Do(func() {
		close(r.done)
		if cerr := r.ps.Close(); cerr != nil {
			err = fmt.Errorf("repository: unsubscribe: %w", cerr)
		}
	})
	return err
}
