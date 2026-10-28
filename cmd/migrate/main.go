// cmd/migrate 是迁移入口：
//
//	migrate up [--target business|eval] [--skip-buckets]   建库 + golang-migrate（业务库另建 MinIO bucket）
//	migrate seed --file configs/seed/fields.yaml          示范农场地块 upsert（可重复执行）
//
// 配置读 configs/app.yaml，环境变量 AS_* 覆盖；密码只从环境变量或 deploy/.env 取。禁止 AutoMigrate。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
)

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
	usage      = "usage: migrate <up|seed> [flags]"

	targetBusiness = "business"
	targetEval     = "eval"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch args[0] {
	case "up":
		err = cmdUp(ctx, args[1:], stdout, stderr)
	case "seed":
		err = cmdSeed(ctx, args[1:], stdout)
	default:
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitUsage
	}
	if err != nil {
		fmt.Fprintln(stderr, "migrate:", err)
		return exitFailed
	}
	return exitOK
}

// common 是各子命令共用的配置参数。
type common struct {
	configFile string
	envFile    string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "configs/app.yaml", "配置文件")
	fs.StringVar(&c.envFile, "env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
}

func (c *common) load() (*config.Config, error) {
	cfg, err := config.Load(c.configFile, c.envFile)
	if err != nil {
		return nil, err
	}
	return cfg, cfg.RequireSecrets("mysql.password")
}

func cmdUp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	var c common
	c.register(fs)
	target := fs.String("target", targetBusiness, "business（agri_steward + bucket）| eval（agri_eval）")
	dir := fs.String("dir", "", "迁移目录（默认 business=migrations，eval=migrations/eval）")
	skipBuckets := fs.Bool("skip-buckets", false, "跳过 MinIO bucket 创建（会在 stderr 明确提示）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	var dbName, migDir string
	switch *target {
	case targetBusiness:
		dbName, migDir = cfg.MySQL.Database, "migrations"
	case targetEval:
		dbName, migDir = cfg.MySQL.EvalDatabase, "migrations/eval"
	default:
		return fmt.Errorf("unknown --target %q", *target)
	}
	if *dir != "" {
		migDir = *dir
	}
	if err := repository.CreateDatabase(ctx, cfg.MySQL.DSN("", false), dbName); err != nil {
		return err
	}
	version, err := repository.Migrate(cfg.MySQL.DSN(dbName, true), migDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "migrated %s to version %d (%s)\n", dbName, version, migDir)
	if *target != targetBusiness {
		return nil
	}
	if *skipBuckets {
		fmt.Fprintln(stderr, "migrate: WARNING buckets skipped by --skip-buckets:", cfg.MinIO.BucketRaw, cfg.MinIO.BucketEval)
		return nil
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return err
	}
	if err := repository.EnsureBuckets(ctx, mc, cfg.MinIO.BucketRaw, cfg.MinIO.BucketEval); err != nil {
		return fmt.Errorf("tables migrated but buckets not created: %w", err)
	}
	fmt.Fprintf(stdout, "buckets ready: %s %s\n", cfg.MinIO.BucketRaw, cfg.MinIO.BucketEval)
	return nil
}

func cmdSeed(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var c common
	c.register(fs)
	file := fs.String("file", "configs/seed/fields.yaml", "种子文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.load()
	if err != nil {
		return err
	}
	data, err := seed.Load(*file)
	if err != nil {
		return err
	}
	logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
	if err != nil {
		return err
	}
	clk := clock.System{}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.Database, clk, logger)
	if err != nil {
		return err
	}
	applyErr := seed.Apply(ctx, db, data, clk.Now())
	if err := errors.Join(applyErr, repository.Close(db)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "seeded farm (%d,%d) %s with %d fields\n",
		data.Farm.TenantID, data.Farm.FarmID, data.Farm.Name, len(data.Fields))
	return nil
}
