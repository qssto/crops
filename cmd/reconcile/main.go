// cmd/reconcile 是 L7 指标对账：对生产业务库（cfg.MySQL.Database）同一 UTC 自然日，
// 分别跑 MetricsService.Daily 与 docs/metrics.md 的直接 COUNT/SUM SQL，逐项比较并打印 Markdown 表。
//
//	reconcile [--day YYYY-MM-DD] [--tenant 1] [--farm 1]
//
// 有任一项不一致时退出码为 1。只读，不写库。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

func main() {
	ok, err := run(os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reconcile:", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) (ok bool, err error) {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	configFile := fs.String("config", "configs/app.yaml", "配置文件")
	envFile := fs.String("env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	dayStr := fs.String("day", "", "对账日 YYYY-MM-DD（UTC，缺省当天）")
	tenant := fs.Uint64("tenant", 1, "tenant_id")
	farm := fs.Uint64("farm", 1, "farm_id")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	clk := clock.System{}
	day := clk.Now().Truncate(24 * time.Hour)
	if *dayStr != "" {
		if day, err = time.ParseInLocation(service.DateLayout, *dayStr, time.UTC); err != nil {
			return false, fmt.Errorf("bad --day: %w", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg, err := config.Load(*configFile, *envFile)
	if err != nil {
		return false, err
	}
	logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
	if err != nil {
		return false, err
	}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.Database, clk, logger)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, repository.Close(db)) }()

	scope := model.Scope{TenantID: *tenant, FarmID: *farm}
	rep, err := service.NewMetricsService(repository.NewMetricsRepo(db), clk).Daily(ctx, scope, day, day)
	if err != nil {
		return false, err
	}
	direct, err := queryDirect(ctx, db, scope, day, day.Add(24*time.Hour))
	if err != nil {
		return false, err
	}
	rows := compare(rep.Days[0], direct)
	fmt.Fprintf(out, "库 %s · tenant %d farm %d · %s（UTC）\n\n", cfg.MySQL.Database, *tenant, *farm, rep.From)
	return printRows(out, rows), nil
}
