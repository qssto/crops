package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

const (
	// DefaultOutboxInterval 是 outbox 轮询周期（P0 §6.9 流程 ①3）。
	DefaultOutboxInterval = time.Second
	// DefaultOutboxLease 是领取 / 续租后推后 next_run_at 的租约（P1 §6.5 续租 SQL 注释 now+60s）。
	DefaultOutboxLease = 60 * time.Second
	// DefaultOutboxRenew 是处理中带 row_ver 续租的周期（P1 §6.5 技术实现第 2 步：每 20s）。
	DefaultOutboxRenew = 20 * time.Second
	// outboxDeferDelay 是拿不到 lock:order（或处理器要求推后）时 next_run_at 的推后量（P1 §6.5 Defer SQL 注释 now+5s）。
	outboxDeferDelay = 5 * time.Second
	// outboxHandleTimeout 是单条消息的处理上限；处理中按 DefaultOutboxRenew 续租，可长于租约。
	outboxHandleTimeout = 2 * time.Minute
)

// outboxBackoff 是第 n 次失败（n 从 1 起）后的重试间隔：1s / 5s / 30s / 2min / 10min；
// 第 5 次失败即 DEAD，10min 只写在 DEAD 行的 next_run_at 上。
var outboxBackoff = [...]time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}

// outboxRetryDelay 返回已失败 attempts 次的消息本次再失败后的退避。
func outboxRetryDelay(attempts int) time.Duration {
	return outboxBackoff[min(max(attempts, 0), len(outboxBackoff)-1)]
}

// outboxSystem 是 outbox 轮询的跨农场凭证（P1-L4 随轮询器迁到 cmd/worker）。
var outboxSystem = repository.NewSystemScope("outbox_poll")

var (
	// ErrUnknownTopic 表示 outbox 消息的 topic 没有处理器（按失败退避，耗尽后 DEAD）。
	ErrUnknownTopic = errors.New("service: unknown outbox topic")
	// ErrOutboxDeferred 由处理器返回（errors.Is），表示本条只推后 next_run_at、不计 attempts（如设备离线，P1 §6.5 ③）。
	// ErrOrderBusy（拿不到 lock:order）同样推后。
	ErrOutboxDeferred = errors.New("service: outbox message deferred")
	// ErrOutboxLeaseLost 是续租影响 0 行时取消处理器 ctx 的 cause：租约已被他人接走，本条不再写结果。
	ErrOutboxLeaseLost = errors.New("service: outbox lease lost")
)

// OutboxStore 是 outbox 领取 / 续租 / 完成 / 推后 / 退避，repository.OutboxRepo 实现；除 Claim 外都带认领时的 row_ver
// 作租约条件，返回 false 即租约已丢。
type OutboxStore interface {
	Claim(ctx context.Context, sys repository.SystemScope, now, leaseUntil time.Time) ([]repository.OutboxMessage, error)
	Renew(ctx context.Context, sys repository.SystemScope, id uint64, rowVer int, leaseUntil, now time.Time) (bool, error)
	MarkDone(ctx context.Context, sys repository.SystemScope, id uint64, rowVer int, now time.Time) (bool, error)
	Defer(ctx context.Context, sys repository.SystemScope, id uint64, rowVer int, nextRunAt, now time.Time) (bool, error)
	MarkRetry(ctx context.Context, sys repository.SystemScope, id uint64, rowVer int, nextRunAt time.Time, lastErr string,
		now time.Time) (bool, error)
}

// AuditAppender 在独立事务内写一条审计，repository.AuditRepo 实现。
type AuditAppender interface {
	Append(ctx context.Context, r model.AuditRecord) error
}

// OutboxHandler 处理一条消息；返回 nil 即置 DONE。
type OutboxHandler func(ctx context.Context, msg repository.OutboxMessage) error

// OutboxDeps 是 OutboxPoller 的依赖。Audit 为空时 DEAD 只记 ERROR 日志。
type OutboxDeps struct {
	Store    OutboxStore
	Handlers map[string]OutboxHandler // topic → 处理器
	Audit    AuditAppender
	Clock    clock.Clock
	Logger   *slog.Logger
	Interval time.Duration // 0 取 DefaultOutboxInterval
	Lease    time.Duration // 0 取 DefaultOutboxLease
	Renew    time.Duration // 0 取 DefaultOutboxRenew
}

// OutboxPoller 是 cmd/worker 内 outbox 轮询器（P1 §6.5 技术实现第 2 步）：多实例靠 FOR UPDATE SKIP LOCKED 互斥，
// 处理中带 row_ver 续租，完成 / 推后 / 退避都带 row_ver 条件。
type OutboxPoller struct{ d OutboxDeps }

// NewOutboxPoller 构造 OutboxPoller。
func NewOutboxPoller(d OutboxDeps) *OutboxPoller {
	if d.Interval <= 0 {
		d.Interval = DefaultOutboxInterval
	}
	if d.Lease <= 0 {
		d.Lease = DefaultOutboxLease
	}
	if d.Renew <= 0 {
		d.Renew = DefaultOutboxRenew
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
// 处理中的消息不随 ctx 取消中断，以免 ⑤-⑦ 半途退出。评测的「同步排空 outbox 一轮」也调用它。
func (p *OutboxPoller) RunOnce(ctx context.Context) (int, error) {
	now := p.d.Clock.Now()
	msgs, err := p.d.Store.Claim(ctx, outboxSystem, now, now.Add(p.d.Lease))
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

// handle 处理一条消息并按租约落 DONE / 推后 / 退避；返回的错误只是记账失败（处理失败已转为退避）。
// 处理期间 keepLease 每 Renew 带 row_ver 续租；续租影响 0 行即租约已丢：取消处理器 ctx，处理返回后不再写任何结果。
func (p *OutboxPoller) handle(ctx context.Context, m repository.OutboxMessage) error {
	base := context.WithoutCancel(ctx)
	tctx, cancelTimeout := context.WithTimeout(base, outboxHandleTimeout)
	defer cancelTimeout()
	hctx, cancel := context.WithCancelCause(tctx)
	defer cancel(nil)
	lease := &outboxLease{rowVer: m.RowVer}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.keepLease(base, m, lease, cancel, stop)
	}()
	herr := p.dispatch(hctx, m)
	close(stop)
	<-done
	attrs := []any{"outbox_id", m.ID, "topic", m.Topic, "aggregate_id", m.AggregateID, "attempts", m.Attempts}
	if lease.lost {
		p.d.Logger.WarnContext(ctx, "outbox lease lost, result dropped", append(attrs, "err", herr)...)
		return nil
	}
	return p.settle(base, m, lease.rowVer, herr, attrs)
}

// settle 按处理结果带 row_ver 写 DONE / 推后 / 退避。
func (p *OutboxPoller) settle(ctx context.Context, m repository.OutboxMessage, rowVer int, herr error, attrs []any) error {
	now := p.d.Clock.Now()
	switch {
	case herr == nil:
		ok, err := p.d.Store.MarkDone(ctx, outboxSystem, m.ID, rowVer, now)
		if err != nil {
			return fmt.Errorf("service: outbox %d done: %w", m.ID, err)
		}
		if !ok {
			p.d.Logger.WarnContext(ctx, "outbox lease lost before done", attrs...)
		}
		return nil
	case errors.Is(herr, ErrOrderBusy) || errors.Is(herr, ErrOutboxDeferred):
		ok, err := p.d.Store.Defer(ctx, outboxSystem, m.ID, rowVer, now.Add(outboxDeferDelay), now)
		if err != nil {
			return errors.Join(fmt.Errorf("service: outbox %d defer: %w", m.ID, err), herr)
		}
		if !ok {
			p.d.Logger.WarnContext(ctx, "outbox lease lost before defer", append(attrs, "err", herr)...)
			return nil
		}
		p.d.Logger.InfoContext(ctx, "outbox message deferred", append(attrs, "err", herr)...)
		return nil
	}
	ok, err := p.d.Store.MarkRetry(ctx, outboxSystem, m.ID, rowVer, now.Add(outboxRetryDelay(m.Attempts)), herr.Error(), now)
	if err != nil {
		return errors.Join(fmt.Errorf("service: outbox %d retry: %w", m.ID, err), herr)
	}
	attrs = append(attrs, "err", herr)
	switch {
	case !ok:
		p.d.Logger.WarnContext(ctx, "outbox message no longer pending", attrs...)
	case m.Attempts+1 >= repository.OutboxMaxAttempts:
		p.d.Logger.ErrorContext(ctx, "outbox message dead", append(attrs, "alert", string(model.AuditOutboxDead))...)
		p.auditDead(ctx, m, herr, now)
	default:
		p.d.Logger.WarnContext(ctx, "outbox message retry", attrs...)
	}
	return nil
}

// outboxLease 是一条消息的租约凭证：只由 keepLease 在处理期间改写，handle 等它退出后再读。
type outboxLease struct {
	rowVer int
	lost   bool
}

// keepLease 每 Renew 带当前 row_ver 续租到 now+Lease；影响 0 行标记丢失并取消处理器（cause ErrOutboxLeaseLost）。
// 续租 SQL 报错（基础设施抖动）只告警、下一周期再试：租约未到期前凭证仍有效。
func (p *OutboxPoller) keepLease(ctx context.Context, m repository.OutboxMessage, l *outboxLease,
	cancel context.CancelCauseFunc, stop <-chan struct{}) {
	t := time.NewTicker(p.d.Renew)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			now := p.d.Clock.Now()
			ok, err := p.d.Store.Renew(ctx, outboxSystem, m.ID, l.rowVer, now.Add(p.d.Lease), now)
			switch {
			case err != nil:
				p.d.Logger.WarnContext(ctx, "outbox lease renew", "outbox_id", m.ID, "err", err)
			case !ok:
				l.lost = true
				cancel(fmt.Errorf("%w: outbox %d row_ver %d", ErrOutboxLeaseLost, m.ID, l.rowVer))
				return
			default:
				l.rowVer++
			}
		}
	}
}

// outboxDead 是 OUTBOX_DEAD 审计的 summary。
type outboxDead struct {
	OutboxID    uint64 `json:"outbox_id"`
	Topic       string `json:"topic"`
	AggregateID uint64 `json:"aggregate_id"`
	DedupKey    string `json:"dedup_key"`
	Attempts    int    `json:"attempts"`
	LastError   string `json:"last_error"`
}

// auditDead 写 OUTBOX_DEAD 审计（消息所属农场，result=FAILED）；写失败只告警（DEAD 已落库，ERROR 日志已告警）。
func (p *OutboxPoller) auditDead(ctx context.Context, m repository.OutboxMessage, herr error, now time.Time) {
	if p.d.Audit == nil {
		return
	}
	payload, err := json.Marshal(outboxDead{OutboxID: m.ID, Topic: m.Topic, AggregateID: m.AggregateID, DedupKey: m.DedupKey,
		Attempts: m.Attempts + 1, LastError: herr.Error()})
	if err == nil {
		rec := repository.NewAuditRecord(model.Scope{TenantID: m.TenantID, FarmID: m.FarmID}, nil,
			model.Actor{Type: model.ActorSystem}, model.AuditOutboxDead, "outbox:"+strconv.FormatUint(m.ID, 10), payload,
			model.AuditResultFailed, now)
		err = p.d.Audit.Append(ctx, rec)
	}
	if err != nil {
		p.d.Logger.WarnContext(ctx, "outbox dead audit", "outbox_id", m.ID, "err", err)
	}
}

// dispatch 按 topic 交处理器；处理器 panic 转为错误（走退避），不拖垮 worker。
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
