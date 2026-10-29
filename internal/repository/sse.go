package repository

import (
	"context"
	"encoding/json"
	"fmt"
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
