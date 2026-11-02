// Package boot 是 cmd/server 与 cmd/worker 共用的进程启动（P1-L4-A）：同一套命令行参数、配置 / 密钥、
// MySQL / Redis / MinIO 连接、MCP gateway、主链路与 app.Build，保证两个进程跑的是同一份装配。
package boot

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

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

// dashboardDir 是看板 amis 页面目录（相对 -repo；被 gitignore，缺失时 /dashboard 返回 404）。
const dashboardDir = "web/amis"

// Flags 是两个进程共用的命令行参数。
type Flags struct {
	Config, EnvFile, Tools, Model, Repo, Taxonomy, Policy, Risk, Permissions, Devices, Regulation string
	NotifyTexts                                                                                   string
}

// RegisterFlags 在 fs 上登记参数（缺省值与 P0 cmd/server 一致）。
func RegisterFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Config, "config", "configs/app.yaml", "配置文件")
	fs.StringVar(&f.EnvFile, "env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	fs.StringVar(&f.Tools, "tools", "configs/tools.yaml", "MCP 工具 Server 配置（mock / real）")
	fs.StringVar(&f.Model, "model", "configs/model.yaml", "模型配置（provider: openai | stub，可用 AS_MODEL_PROVIDER 覆盖）")
	fs.StringVar(&f.Repo, "repo", ".", "仓库根目录（其下 knowledge/ 为知识库）")
	fs.StringVar(&f.Taxonomy, "taxonomy", "eval/taxonomy.yaml", "question_taxonomy 文件（分类与知识加载共用）")
	fs.StringVar(&f.Policy, "policy", "policy/tool_policy.yaml", "工具策略（与工具注册表启动期比对，不符即退出）")
	fs.StringVar(&f.Risk, "risk", "policy/risk_matrix.yaml", "审批风险矩阵（作业类型 → risk_level / required_role）")
	fs.StringVar(&f.Permissions, "permissions", "policy/permission_matrix.yaml", "权限矩阵（角色 → 操作，未声明即拒绝）")
	fs.StringVar(&f.Devices, "devices", "configs/devices.yaml", "设备 Adapter 注册表配置（devicesim 开关；骨架不可开放）")
	fs.StringVar(&f.Regulation, "regulation", "policy/regulation_map.yaml", "法规对照表（P1 L3，rule_id 启动期校验）")
	fs.StringVar(&f.NotifyTexts, "notify-texts", "configs/texts/notify.zh-CN.yaml", "通知文案（P1 L5，缺任一通知类型即启动失败）")
	return f
}

// Process 是启动好的进程依赖；Close 关闭 Redis 与 MySQL。
type Process struct {
	App    *app.App
	Config *config.Config
	Model  *config.Model
	db     *gorm.DB
	rdb    redis.UniversalClient
}

// Close 关闭连接（Redis 先于 MySQL）。
func (p *Process) Close() error {
	var errs []error
	if err := p.rdb.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close redis: %w", err))
	}
	return errors.Join(append(errs, repository.Close(p.db))...)
}

// Open 读配置与密钥 → 选模型 → 连 MySQL / Redis / MinIO → 装配工具与主链路 → app.Build。
// 失败时已打开的连接一并关闭。密钥只经 config 从环境变量 / deploy/.env 读取，不打印。
func Open(ctx context.Context, f *Flags) (_ *Process, err error) {
	cfg, err := config.Load(f.Config, f.EnvFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireSecrets("mysql.password", "redis.password", "minio.access_key", "minio.secret_key",
		"auth.jwt_secret"); err != nil {
		return nil, err
	}
	// 模型在连库之前选定：openai 缺 ONEAPI_* 时直接报错退出。
	mcfg, err := config.LoadModel(f.Model)
	if err != nil {
		return nil, err
	}
	chat, err := app.ChatModel(mcfg)
	if err != nil {
		return nil, err
	}
	tx, err := knowledge.LoadTaxonomy(f.Taxonomy)
	if err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(os.Stderr, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	clk := clock.System{}
	keys, err := rediskey.New(cfg.Env)
	if err != nil {
		return nil, err
	}
	grants, err := service.LoadPermissionMatrix(f.Permissions)
	if err != nil {
		return nil, err
	}
	notifyTexts, err := service.LoadNotifyTexts(f.NotifyTexts)
	if err != nil {
		return nil, err
	}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.Database, clk, logger)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, repository.Close(db))
		}
	}()
	rdb, err := app.OpenRedis(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if cerr := rdb.Close(); cerr != nil {
				err = errors.Join(err, fmt.Errorf("close redis: %w", cerr))
			}
		}
	}()
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return nil, err
	}
	raw := repository.NewRawStore(db, repository.MinIOPutter{Client: mc}, cfg.MinIO.BucketRaw)
	rawRead := repository.RawReader{DB: db, Obj: repository.MinIOGetter{Client: mc}, Bucket: cfg.MinIO.BucketRaw}
	tools, err := buildTools(f.Tools, f.Policy, toolDeps{clk: clk, logger: logger, db: db, rdb: rdb, keys: keys,
		raw: raw, rawRead: rawRead})
	if err != nil {
		return nil, err
	}
	risk, err := policy.LoadRiskMatrix(f.Risk)
	if err != nil {
		return nil, err
	}
	devices, err := config.LoadDevices(f.Devices)
	if err != nil {
		return nil, err
	}
	constraints, err := app.LoadConstraintReviewer(f.Regulation)
	if err != nil {
		return nil, err
	}
	history, err := fieldHistoryWriter(f.Tools)
	if err != nil {
		return nil, err
	}
	a, err := app.Build(app.Deps{Clock: clk, Logger: logger, DB: db, Redis: rdb, Keys: keys, RawStore: raw,
		NewAgent: app.GraphAgent(ctx, app.GraphConfig{Model: mcfg, Chat: chat, Taxonomy: tx,
			KB: knowledge.NewStore(knowledge.NewRepoReader(f.Repo))}),
		Tools: tools, Risk: risk, Constraints: constraints, Permissions: grants, Devices: devices, Env: cfg.Env,
		Dispatch:      cfg.Dispatch,
		Approval:      cfg.Approval,                     // P1 L5：驳回导出跨度上限
		NotifyTexts:   notifyTexts,                      // P1 L5：notify.send 文案
		ReceiptSecret: []byte(cfg.DeviceReceipt.Secret), // P1-L4-B：为空不挂回执 webhook（只有 server 挂路由）
		FieldHistory:  history,                          // P1-L4-B：field.history.append 由 worker 的 outbox 处理器写
		DashboardDir:  filepath.Join(f.Repo, dashboardDir),
		Presign:       repository.MinIOPresigner{Client: mc, Bucket: cfg.MinIO.BucketRaw},
		HITL: app.HITLDeps{Objects: repository.MinIOGetter{Client: mc}, RawBucket: cfg.MinIO.BucketRaw,
			Memory: memory.Options{Enabled: cfg.Memory.Enabled, EpisodicLimit: cfg.Memory.EpisodicLimit,
				RuneBudget: cfg.Memory.RuneBudget}}})
	if err != nil {
		return nil, err
	}
	logger.Info("advice graph ready", "provider", mcfg.Provider, "tier", mcfg.DefaultTier, "prompt_version", mcfg.PromptVersion)
	return &Process{App: a, Config: cfg, Model: mcfg, db: db, rdb: rdb}, nil
}
