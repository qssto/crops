// cmd/server 是 HTTP 服务入口：boot.Open（配置与密钥 → MySQL / Redis / MinIO → app.Build）→ gin 路由。
// 所有路由在 /api/v1/farms/:farm_id/ 下并经鉴权中间件。P1-L4-A 起 outbox 轮询、扫描任务与分区维护在 cmd/worker，
// server 只跑 HTTP、SSE 与开关订阅。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/app/boot"
)

const (
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
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
	a := p.App
	auth, err := a.NewAuth(p.Config.Auth.JWTSecret)
	if err != nil {
		return err
	}
	metrics := a.MetricsLoop(p.Config.Metrics.ServerAddr, p.Config.Metrics.Path) // P1 L7-B：独立监听，不经业务鉴权
	return serve(ctx, &http.Server{Addr: p.Config.HTTP.Addr, Handler: a.Handler(auth), ReadHeaderTimeout: readHeaderTimeout}, a,
		metrics)
}

// serve 启动 server 侧后台任务（app.ServerLoops：只有开关订阅；outbox 轮询、扫描任务与分区维护在 cmd/worker），
// 并监听直到 ctx 取消，然后优雅关闭。
func serve(ctx context.Context, srv *http.Server, a *app.App, extra ...app.Loop) error {
	waitLoops := app.StartLoops(ctx, append(a.ServerLoops(), extra...))
	// SSE 长连接不随 Shutdown 结束，关闭开始时让推送返回
	srv.RegisterOnShutdown(a.EventStream.Shutdown)
	errCh := make(chan error, 1)
	go func() {
		a.Logger.Info("http listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return fmt.Errorf("listen %s: %w", srv.Addr, err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen %s: %w", srv.Addr, err)
	}
	// 等待已受理的异步主链路、开关订阅与审批后的异步续跑收尾（各自带超时兜底）。
	a.Ask.Wait()
	waitLoops()
	a.HITL.Decisions.Wait()
	return nil
}
