package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// ErrOrderLockLost 是 lock:order 续租失败时取消持锁 ctx 的 cause：锁可能已被他人拿到，持锁方必须停下（P1 §6.5 第 7 条）。
var ErrOrderLockLost = errors.New("service: order lock lost")

// orderLockRenewEvery 是持锁期间续租周期（ttl/3）；包级变量便于单测缩短。
var orderLockRenewEvery = orderLockTTL / 3

// WithOrderLock 持 lock:order 执行 fn（P1-L4 下发器 ⑤-⑦、Reopen、L4-B 回执 / 中止共用）：
//   - 拿不到锁返回包装 ErrOrderBusy 的错误（outbox 轮询器据此推后 5s、不计 attempts）；
//   - 持锁期间每 ttl/3 用 Lua 校验 token 续租，续租失败即以 ErrOrderLockLost 取消 fn 的 ctx 并停止续租；
//   - fn 返回后释放锁；锁曾丢失时返回包装 ErrOrderLockLost 的错误（与 fn 的错误合并）。
//
// fn 内的 DB / 设备 IO 必须使用传入的 ctx，锁丢失后才会及时中断。
func WithOrderLock(ctx context.Context, locks KeyLocker, keys rediskey.Keys, log *slog.Logger, orderID uint64,
	fn func(ctx context.Context) error) error {
	lease, err := locks.AcquireLease(ctx, keys.LockOrder(orderID), orderLockTTL)
	if err != nil {
		return lockAcquireErr(orderID, err)
	}
	lctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := make(chan struct{})
	exited := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(exited)
		t := time.NewTicker(orderLockRenewEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if rerr := lease.Renew(context.WithoutCancel(lctx)); rerr != nil {
					cause := fmt.Errorf("%w: order %d: %w", ErrOrderLockLost, orderID, rerr)
					lost <- cause
					cancel(cause)
					return
				}
			}
		}
	}()
	ferr := fn(lctx)
	close(stop)
	<-exited
	if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
		log.WarnContext(ctx, "release order lock", "order_id", orderID, "err", rerr)
	}
	select {
	case cause := <-lost:
		log.ErrorContext(ctx, "order lock lost, holder stopped", "order_id", orderID, "err", cause)
		return errors.Join(cause, ferr)
	default:
		return ferr
	}
}

// lockAcquireErr 把 ErrLockHeld 映射为 ErrOrderBusy，其余错误照常包装（与 acquireOrderLock 一致）。
func lockAcquireErr(orderID uint64, err error) error {
	if errors.Is(err, repository.ErrLockHeld) {
		return fmt.Errorf("service: order %d: %w", orderID, ErrOrderBusy)
	}
	return fmt.Errorf("service: lock order %d: %w", orderID, err)
}
