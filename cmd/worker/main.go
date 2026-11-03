// cmd/worker 是后台任务进程（P1-L4-A）：与 cmd/server 共用 boot.Open（同一套配置 / 密钥 / app.Build），
// 运行 app.WorkerLoops：outbox 轮询（order.dispatch + notify.send 占位 + OrderOpsHandlers）、P0 扫描任务、
// 分区维护、开关订阅与 OrderOpsLoops。多副本安全（领取 FOR UPDATE SKIP LOCKED、lock:scan:{task}、lock:order）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"agri-steward/internal/app"
	"agri-steward/internal/app/boot"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	flags := boot.RegisterFlags(flag.CommandLine)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p, err := boot.Open(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, p.Close()) }()
	return work(ctx, p.App, p.App.MetricsLoop(p.Config.Metrics.WorkerAddr, p.Config.Metrics.Path)) // P1 L7-B
}

// work 装配后台任务（处理器表 topic 冲突即启动失败），运行到 ctx 取消，等全部任务返回后收尾。
func work(ctx context.Context, a *app.App, extra ...app.Loop) error {
	loops, err := a.WorkerLoops()
	if err != nil {
		return err
	}
	loops = append(loops, extra...)
	names := make([]string, 0, len(loops))
	for _, l := range loops {
		names = append(names, l.Name)
	}
	a.Logger.Info("worker started", "loops", names)
	wait := app.StartLoops(ctx, loops)
	<-ctx.Done()
	wait()
	// 下发器复检退回等路径可能触发审批后的异步续跑，退出前等其收尾（自带超时兜底）。
	a.HITL.Decisions.Wait()
	a.Logger.Info("worker stopped")
	return nil
}
