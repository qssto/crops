// cmd/server 是 HTTP 服务入口：读配置与密钥 → 连 MySQL / Redis / MinIO → app.Build → gin 路由。
// 所有路由在 /api/v1/farms/:farm_id/ 下并经鉴权中间件；L1 只有 GET ping。
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

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service"
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
	configFile := flag.String("config", "configs/app.yaml", "配置文件")
	envFile := flag.String("env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*configFile, *envFile)
	if err != nil {
		return err
	}
	if err := cfg.RequireSecrets("mysql.password", "redis.password", "minio.access_key", "minio.secret_key",
		"auth.jwt_secret"); err != nil {
		return err
	}
	logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
	if err != nil {
		return err
	}
	clk := clock.System{}
	keys, err := rediskey.New(cfg.Env)
	if err != nil {
		return err
	}
	grants, err := config.LoadGrants(cfg.Auth.File)
	if err != nil {
		return err
	}
	auth, err := service.NewAuthService(cfg.Auth.JWTSecret, grants, clk)
	if err != nil {
		return err
	}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.Database, clk, logger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, repository.Close(db)) }()
	rdb, err := app.OpenRedis(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := rdb.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close redis: %w", cerr))
		}
	}()
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return err
	}
	a, err := app.Build(app.Deps{Clock: clk, Logger: logger, DB: db, Redis: rdb, Keys: keys,
		RawStore: repository.NewRawStore(db, repository.MinIOPutter{Client: mc}, cfg.MinIO.BucketRaw),
		// L4 前主链路未就绪，注入只拒答的占位实现。
		Agent: agent.Placeholder{}})
	if err != nil {
		return err
	}
	return serve(ctx, &http.Server{Addr: cfg.HTTP.Addr, Handler: a.Handler(auth), ReadHeaderTimeout: readHeaderTimeout}, a)
}

// serve 监听直到 ctx 取消，然后优雅关闭。
func serve(ctx context.Context, srv *http.Server, a *app.App) error {
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
	return nil
}
