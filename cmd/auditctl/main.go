// cmd/auditctl 是审计链离线校验工具（P1 L7 §6.8 技术实现第 4 条），只读连接（SET transaction_read_only=1）：
//
//	auditctl verify --farm <id> [--tenant <id>] [--from-seq N --to-seq M]
//	    按 chain_seq 流式重算，报第一个 ROW_MODIFIED / GAP / LINK_BROKEN（含 chain_seq 与 audit_log.id），有断点退出码 1。
//	auditctl anchor-check --date yyyymmdd [--anchor-file path]
//	    比对锚点 anchor/{date}.json 与当前链（截尾后重算 verify 本身发现不了），不一致退出码 1。
//	auditctl verify --from-manifest <path>
//	    只解析参数，返回「未启用」（导出包离线校验留给后续）。
//
// 配置读 configs/app.yaml；密钥只从环境变量取（不读 deploy/.env），任何输出都不含密钥。历史 chain_seq IS NULL 行不参与校验。
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
)

const (
	exitOK      = 0
	exitBroken  = 1 // 发现断点 / 锚点不一致
	exitUsage   = 2
	exitFailed  = 3 // 运行失败（连库、读对象等）
	usage       = "usage: auditctl <verify|anchor-check> [flags]"
	notEnabled  = "auditctl: --from-manifest 未启用（导出包离线校验留给后续阶段）"
	multiTenant = "farm %d belongs to tenants %v; pass --tenant"
)

// errBroken 表示校验完成且发现问题（退出码 1，与运行失败区分）。
var errBroken = errors.New("audit chain check failed")

// errNotEnabled 表示请求了未启用的功能。
var errNotEnabled = errors.New(notEnabled)

// errUsage 表示参数不合法（退出码 2）。
var errUsage = errors.New("invalid arguments")

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
	case "verify":
		err = cmdVerify(ctx, args[1:], stdout)
	case "anchor-check":
		err = cmdAnchorCheck(ctx, args[1:], stdout)
	default:
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitUsage
	case errors.Is(err, errNotEnabled), errors.Is(err, errUsage):
		fmt.Fprintln(stderr, "auditctl:", err)
		return exitUsage
	case errors.Is(err, errBroken):
		fmt.Fprintln(stderr, "auditctl:", err)
		return exitBroken
	default:
		fmt.Fprintln(stderr, "auditctl:", err)
		return exitFailed
	}
}

// common 是各子命令共用的参数。
type common struct{ configFile string }

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configFile, "config", "configs/app.yaml", "配置文件（密钥只从环境变量取）")
}

// load 读配置（不读 deploy/.env）。
func (c *common) load() (*config.Config, error) {
	cfg, err := config.Load(c.configFile, "")
	if err != nil {
		return nil, err
	}
	return cfg, cfg.RequireSecrets("mysql.password")
}

// openReadOnly 打开只读会话的业务库连接。
func openReadOnly(ctx context.Context, cfg *config.Config) (*repository.DB, error) {
	dsn, err := cfg.MySQL.ReadOnlyDSN(cfg.MySQL.Database)
	if err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	return repository.OpenMySQL(ctx, repository.OpenOptions{DSN: dsn, MaxOpenConns: 2, Clock: clock.System{}, Logger: logger})
}
