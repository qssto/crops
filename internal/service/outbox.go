package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/repository"
)

const (
	// DefaultOutboxInterval 是 outbox 轮询周期（P0 §6.9 流程 ①3）。
	DefaultOutboxInterval = time.Second
	// DefaultOutboxLease 是领取后推后 next_run_at 的租约（同上）。
	DefaultOutboxLease = 60 * time.Second
	// outboxHandleTimeout 是单条消息的处理上限（须小于租约，避免租约过期被他人重复领取）。
	outboxHandleTimeout = 30 * time.Second
)

// outboxBackoff 是第 n 次失败（n 从 1 起）后的重试间隔：1s / 5s / 30s / 2min / 10min；
// 第 5 次失败即 DEAD，10min 只写在 DEAD 行的 next_run_at 上。
var outboxBackoff = [...]time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// outboxRetryDelay 返回已失败 attempts 次的消息本次再失败后的退避。
func outboxRetryDelay(attempts int) time.Duration {
	return outboxBackoff[min(max(attempts, 0), len(outboxBackoff)-1)]
}

// ErrUnknownTopic 表示 outbox 消息的 topic 没有处理器（按失败退避，耗尽后 DEAD）。
var ErrUnknownTopic = errors.New("service: unknown outbox topic")

// OutboxStore 是 outbox 领取 / 完成 / 退避，repository.OutboxRepo 实现。
type OutboxStore interface {
	Claim(ctx context.Context, now, leaseUntil time.Time) ([]repository.OutboxMessage, error)
	MarkDone(ctx context.Context, id uint64, now time.Time) (bool, error)
	MarkRetry(ctx context.Context, id uint64, nextRunAt time.Time, lastErr string, now time.Time) (bool, error)
}

// OutboxHandler 处理一条消息；返回 nil 即置 DONE。
type OutboxHandler func(ctx context.Context, msg repository.OutboxMessage) error

// OutboxDeps 是 OutboxPoller 的依赖。
type OutboxDeps struct {
	Store    OutboxStore
	Handlers map[string]OutboxHandler // topic → 处理器
	Clock    clock.Clock
	Logger   *slog.Logger
	Interval time.Duration // 0 取 DefaultOutboxInterval
	Lease    time.Duration // 0 取 DefaultOutboxLease
}

// OutboxPoller 是 server 内 outbox 轮询器（P1 迁到 cmd/worker）：多实例靠 FOR UPDATE SKIP LOCKED 互斥。
type OutboxPoller struct{ d OutboxDeps }

// NewOutboxPoller 构造 OutboxPoller。
func NewOutboxPoller(d OutboxDeps) *OutboxPoller {
	if d.Interval <= 0 {
		d.Interval = DefaultOutboxInterval
	}
	if d.Lease <= 0 {
		d.Lease = DefaultOutboxLease
	}
	return &OutboxPoller{d: d}
}

// Run 每个周期同步处理一轮，直到 ctx 取消；返回时没有进行中的消息。
func (p *OutboxPoller) Run(ctx context.Context) {
	t := time.NewTicker(p.d.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := p.RunOnce(ctx); err != nil {
				p.d.Logger.WarnContext(ctx, "outbox round", "err", err)
			}
		}
	}
}

// RunOnce 领取一批到期消息并逐条处理，返回处理条数。ctx 取消后不再开始新消息（已领取的待租约到期后重领）；
// 处理中的消息不随 ctx 取消中断，以免 ⑤-⑦ 半途退出。
func (p *OutboxPoller) RunOnce(ctx context.Context) (int, error) {
	now := p.d.Clock.Now()
	msgs, err := p.d.Store.Claim(ctx, now, now.Add(p.d.Lease))
	if err != nil {
		return 0, fmt.Errorf("service: outbox claim: %w", err)
	}
	var errs []error
	n := 0
	for _, m := range msgs {
		if ctx.Err() != nil {
			break
		}
		if err := p.handle(ctx, m); err != nil {
			errs = append(errs, err)
		}
		n++
	}
	return n, errors.Join(errs...)
}

// handle 处理一条消息并落 DONE / 退避；返回的错误只是记账失败（处理失败已转为退避）。
func (p *OutboxPoller) handle(ctx context.Context, m repository.OutboxMessage) error {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outboxHandleTimeout)
	defer cancel()
	herr := p.dispatch(hctx, m)
	now := p.d.Clock.Now()
	if herr == nil {
		if _, err := p.d.Store.MarkDone(hctx, m.ID, now); err != nil {
			return fmt.Errorf("service: outbox %d done: %w", m.ID, err)
		}
		return nil
	}
	ok, err := p.d.Store.MarkRetry(hctx, m.ID, now.Add(outboxRetryDelay(m.Attempts)), herr.Error(), now)
	if err != nil {
		return errors.Join(fmt.Errorf("service: outbox %d retry: %w", m.ID, err), herr)
	}
	attrs := []any{"outbox_id", m.ID, "topic", m.Topic, "aggregate_id", m.AggregateID, "attempts", m.Attempts + 1, "err", herr}
	switch {
	case !ok:
		p.d.Logger.WarnContext(ctx, "outbox message no longer pending", attrs...)
	case m.Attempts+1 >= repository.OutboxMaxAttempts:
		p.d.Logger.ErrorContext(ctx, "outbox message dead", attrs...)
	default:
		p.d.Logger.WarnContext(ctx, "outbox message retry", attrs...)
	}
	return nil
}

// dispatch 按 topic 交处理器；处理器 panic 转为错误（走退避），不拖垮 server。
func (p *OutboxPoller) dispatch(ctx context.Context, m repository.OutboxMessage) (err error) {
	h, ok := p.d.Handlers[m.Topic]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTopic, m.Topic)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("service: outbox %d handler panic: %v", m.ID, r)
		}
	}()
	return h(ctx, m)
}
