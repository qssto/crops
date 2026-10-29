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
	"path/filepath"
	"syscall"
	"time"

	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
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
	// dashboardDir 是看板 amis 页面目录（相对 -repo；被 gitignore，缺失时 /dashboard 返回 404）。
	dashboardDir = "web/amis"
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
	toolsFile := flag.String("tools", "configs/tools.yaml", "MCP 工具 Server 配置（mock / real）")
	modelFile := flag.String("model", "configs/model.yaml", "模型配置（provider: openai | stub，可用 AS_MODEL_PROVIDER 覆盖）")
	repoRoot := flag.String("repo", ".", "仓库根目录（其下 knowledge/ 为知识库）")
	taxonomyFile := flag.String("taxonomy", "eval/taxonomy.yaml", "question_taxonomy 文件（分类与知识加载共用）")
	policyFile := flag.String("policy", "policy/tool_policy.yaml", "工具策略（与工具注册表启动期比对，不符即退出）")
	riskFile := flag.String("risk", "policy/risk_matrix.yaml", "审批风险矩阵（作业类型 → risk_level / required_role）")
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
	// 模型在连库之前选定：openai 缺 ONEAPI_* 时直接报错退出。
	mcfg, err := config.LoadModel(*modelFile)
	if err != nil {
		return err
	}
	chat, err := app.ChatModel(mcfg)
	if err != nil {
		return err
	}
	tx, err := knowledge.LoadTaxonomy(*taxonomyFile)
	if err != nil {
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
	raw := repository.NewRawStore(db, repository.MinIOPutter{Client: mc}, cfg.MinIO.BucketRaw)
	rawRead := repository.RawReader{DB: db, Obj: repository.MinIOGetter{Client: mc}, Bucket: cfg.MinIO.BucketRaw}
	tools, err := buildTools(*toolsFile, *policyFile, toolDeps{clk: clk, logger: logger, db: db, rdb: rdb, keys: keys,
		raw: raw, rawRead: rawRead})
	if err != nil {
		return err
	}
	risk, err := policy.LoadRiskMatrix(*riskFile)
	if err != nil {
		return err
	}
	a, err := app.Build(app.Deps{Clock: clk, Logger: logger, DB: db, Redis: rdb, Keys: keys, RawStore: raw,
		NewAgent: app.GraphAgent(ctx, app.GraphConfig{Model: mcfg, Chat: chat, Taxonomy: tx,
			KB: knowledge.NewStore(knowledge.NewRepoReader(*repoRoot))}),
		Tools: tools, Risk: risk, DashboardDir: filepath.Join(*repoRoot, dashboardDir),
		Presign: repository.MinIOPresigner{Client: mc, Bucket: cfg.MinIO.BucketRaw},
		HITL: app.HITLDeps{Objects: repository.MinIOGetter{Client: mc}, RawBucket: cfg.MinIO.BucketRaw,
			Memory: memory.Options{Enabled: cfg.Memory.Enabled, EpisodicLimit: cfg.Memory.EpisodicLimit,
				RuneBudget: cfg.Memory.RuneBudget}}})
	if err != nil {
		return err
	}
	logger.Info("advice graph ready", "provider", mcfg.Provider, "tier", mcfg.DefaultTier, "prompt_version", mcfg.PromptVersion)
	return serve(ctx, &http.Server{Addr: cfg.HTTP.Addr, Handler: a.Handler(auth), ReadHeaderTimeout: readHeaderTimeout}, a)
}

// serve 启动 L6 扫描任务（approval_expire / ticket_activate / run_finalize / stuck_run）并监听直到 ctx 取消，然后优雅关闭。
func serve(ctx context.Context, srv *http.Server, a *app.App) error {
	// Scanner.Run 退出前自行等待其异步 Resume；这里等它返回，避免在扫描轮次中途与 wg.Add 并发 Wait。
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		a.HITL.Scanner.Run(ctx)
	}()
	// L7 分区维护（每月 25 日起预建后两个月；lock:scan:partition 跨实例互斥），退出时等待。
	partDone := make(chan struct{})
	go func() {
		defer close(partDone)
		a.Partitions.Run(ctx)
	}()
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
	// 等待已受理的异步主链路、扫描任务与审批后的异步续跑收尾（各自带超时兜底）。
	a.Ask.Wait()
	<-scanDone
	<-partDone
	a.HITL.Decisions.Wait()
	return nil
}
