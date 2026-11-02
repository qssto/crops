package app

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1-L4-A：cmd/server 与 cmd/worker 的后台任务划分（§6.5 功能任务 1）。
//   - server：HTTP、SSE、开关订阅（Flags.Run）；Ask.Wait / Decisions.Wait 由 cmd/server 收尾。
//   - worker：outbox 轮询器（order.dispatch + notify.send（P1 L5 处理器）+ OrderOpsHandlers）、P0 扫描任务、分区维护、开关订阅、
//     OrderOpsLoops（L4-B 回执超时扫描等）。多副本安全：outbox 靠 FOR UPDATE SKIP LOCKED + row_ver 租约，
//     扫描 / 分区靠 lock:scan:{task}，下发靠 lock:order + idem_key。

// Loop 是一个随进程运行到 ctx 取消的后台任务；Run 返回即任务已停止（调用方等待全部返回再退出）。
type Loop struct {
	Name string
	Run  func(ctx context.Context)
}

// 后台任务名（日志与任务划分测试用）。
const (
	LoopOutbox     = "outbox"
	LoopScanner    = "scanner"
	LoopPartitions = "partitions"
	LoopFlags      = "flags"
	LoopOrderOps   = "orderops"
)

// OutboxHandlers 是 worker 的 outbox 处理器表：order.dispatch（下发器）+ notify.send（P1 L5 NotifyHandler）+
// a.OrderOpsHandlers()（L4-B）。topic 重复、处理器为空或未注入通知文案返回错误（cmd/worker 启动失败）。评测排空 outbox 也用这张表。
func (a *App) OutboxHandlers() (map[string]service.OutboxHandler, error) {
	notify, err := a.notifyHandler()
	if err != nil {
		return nil, err
	}
	return MergeOutboxHandlers(map[string]service.OutboxHandler{
		repository.TopicOrderDispatch: a.WorkOrders.Dispatch,
		repository.TopicNotifySend:    notify,
	}, a.OrderOpsHandlers())
}

// MergeOutboxHandlers 合并多张处理器表；同一 topic 出现两次或处理器为 nil 即报错（不静默覆盖）。
func MergeOutboxHandlers(tables ...map[string]service.OutboxHandler) (map[string]service.OutboxHandler, error) {
	out := map[string]service.OutboxHandler{}
	for _, t := range tables {
		for _, topic := range slices.Sorted(maps.Keys(t)) {
			h := t[topic]
			if _, dup := out[topic]; dup {
				return nil, fmt.Errorf("app: outbox topic %q registered twice", topic)
			}
			if h == nil || topic == "" {
				return nil, fmt.Errorf("app: outbox topic %q has no handler", topic)
			}
			out[topic] = h
		}
	}
	return out, nil
}

// NewOutboxPoller 按 OutboxHandlers 构造 outbox 轮询器（DEAD 写 OUTBOX_DEAD 审计）。cmd/worker 与评测排空共用。
func (a *App) NewOutboxPoller() (*service.OutboxPoller, error) {
	handlers, err := a.OutboxHandlers()
	if err != nil {
		return nil, err
	}
	return service.NewOutboxPoller(service.OutboxDeps{Store: a.outboxStore, Handlers: handlers,
		Audit: repository.AuditRepo{DB: a.DB}, Clock: a.Clock, Logger: a.Logger}), nil
}

// ServerLoops 是 cmd/server 的后台任务：只有开关订阅（outbox 轮询与扫描任务已迁到 cmd/worker）。
func (a *App) ServerLoops() []Loop {
	return []Loop{{Name: LoopFlags, Run: a.Flags.Run}}
}

// WorkerLoops 是 cmd/worker 的后台任务：outbox 轮询、P0 扫描（approval_expire / ticket_activate / run_finalize / stuck_run）、
// 分区维护、开关订阅与 OrderOpsLoops。处理器表 topic 冲突返回错误。
func (a *App) WorkerLoops() ([]Loop, error) {
	poller, err := a.NewOutboxPoller()
	if err != nil {
		return nil, err
	}
	loops := []Loop{
		{Name: LoopOutbox, Run: poller.Run},
		{Name: LoopScanner, Run: a.HITL.Scanner.Run},
		{Name: LoopPartitions, Run: a.Partitions.Run},
		{Name: LoopFlags, Run: a.Flags.Run},
	}
	for i, run := range a.OrderOpsLoops() {
		if run == nil {
			return nil, fmt.Errorf("app: order ops loop %d is nil", i)
		}
		loops = append(loops, Loop{Name: fmt.Sprintf("%s#%d", LoopOrderOps, i), Run: run})
	}
	return loops, nil
}

// StartLoops 为每个任务起一个 goroutine，返回的 wait 阻塞到全部返回（ctx 取消后调用）。
func StartLoops(ctx context.Context, loops []Loop) (wait func()) {
	done := make([]chan struct{}, len(loops))
	for i, l := range loops {
		done[i] = make(chan struct{})
		go func() {
			defer close(done[i])
			l.Run(ctx)
		}()
	}
	return func() {
		for _, d := range done {
			<-d
		}
	}
}
