package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
)

// infraFlags 是 run 连接真实 MySQL / Redis / MinIO 的参数。
type infraFlags struct {
	configFile string
	envFile    string
	migrations string
	seedFile   string
	modelFile  string
	promptsDir string
	riskFile   string
	poolSize   int
}

func (f *infraFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.configFile, "config", "configs/app.yaml", "配置文件")
	fs.StringVar(&f.envFile, "env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	fs.StringVar(&f.migrations, "migrations", "migrations", "trial schema 使用的业务库迁移目录")
	fs.StringVar(&f.seedFile, "seed", "configs/seed/fields.yaml", "每个 trial 重灌的种子文件")
	fs.StringVar(&f.modelFile, "model", "configs/model.yaml", "模型配置（提示词与档位；评测固定用桩模型）")
	fs.StringVar(&f.promptsDir, "prompts", "", "提示词根目录（空则取模型配置的 prompts_dir）")
	fs.StringVar(&f.riskFile, "risk", "policy/risk_matrix.yaml", "审批风险矩阵（与生产同一份）")
	fs.IntVar(&f.poolSize, "pool", 1, "trial schema 槽位数（agri_eval_t_1..n）")
}

// infra 是一次评测持有的外部资源。
type infra struct {
	evalDB *repository.DB
	rdb    *redis.Client
	pool   *env.Pool
	deps   evalkit.DepsFunc
	// question 把样本情境解析为种子农场下的 Scope / FieldID。
	question evalkit.QuestionFunc
	// objects / evalBucket 供 L7 transcript 上传（cfg.MinIO.BucketEval）。
	objects    repository.ObjectPutter
	evalBucket string
	// model 是实际装配用的模型配置（provider 已强制为桩），L8 写入配置快照。
	model *config.Model
}

// open 依次连 agri_eval、Redis、trial 池；任一不可用都返回错误（不静默跳过）。
// 主链路用 ls 的知识库与 taxonomy，模型固定为确定性桩（评测可复现，不依赖外部模型）。
func (f *infraFlags) open(ctx context.Context, logW io.Writer, ls *loadedSamples) (_ *infra, err error) {
	cfg, err := config.Load(f.configFile, f.envFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireSecrets("mysql.password", "redis.password", "minio.access_key", "minio.secret_key"); err != nil {
		return nil, err
	}
	mcfg, err := config.LoadModel(f.modelFile)
	if err != nil {
		return nil, err
	}
	mcfg.Provider = config.ModelProviderStub
	if f.promptsDir != "" {
		mcfg.PromptsDir = f.promptsDir
	}
	chat, err := app.ChatModel(mcfg)
	if err != nil {
		return nil, err
	}
	data, err := seed.Load(f.seedFile)
	if err != nil {
		return nil, err
	}
	risk, err := policy.LoadRiskMatrix(f.riskFile)
	if err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(logW, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	in := &infra{model: mcfg}
	defer func() {
		if err != nil {
			err = errors.Join(err, in.close())
		}
	}()
	if in.evalDB, err = app.OpenMySQL(ctx, cfg, cfg.MySQL.EvalDatabase, clock.System{}, logger); err != nil {
		return nil, err
	}
	if in.rdb, err = app.OpenRedis(ctx, cfg); err != nil {
		return nil, err
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return nil, err
	}
	in.pool, err = env.NewPool(ctx, env.PoolConfig{MySQL: cfg.MySQL, MigrationsDir: f.migrations, Seed: data,
		Redis: in.rdb, Logger: logger, Size: f.poolSize})
	if err != nil {
		return nil, err
	}
	putter := repository.MinIOPutter{Client: mc}
	in.objects, in.evalBucket = putter, cfg.MinIO.BucketEval
	tenantID, farmID := data.Farm.TenantID, data.Farm.FarmID
	in.question = seedQuestion(tenantID, farmID)
	in.deps = func(ctx context.Context, s *env.Slot, smp *sample.Sample) (app.Deps, error) {
		// 样本 match.field 的 field-00x 按种子农场在本 trial schema 中解析为 field_profile.id。
		resolve := func(ctx context.Context, code string) (uint64, error) {
			return repository.ProfileRepo{}.FieldIDByCode(ctx, s.DB, tenantID, farmID, code)
		}
		tools, err := env.NewFixtureMCP(ctx, smp.Fixtures.MCP, resolve, s.Clock)
		if err != nil {
			return app.Deps{}, err
		}
		// 每 trial 一个日志捕获（pii_clean 扫描脱敏后实际写出的内容）与 RecordingAdapter（stop_at_approval）。
		trialLog, err := app.NewTappedLogger(logW, cfg.Log.Level, env.NewLogCapture().Tap)
		if err != nil {
			return app.Deps{}, err
		}
		mem, err := env.NewFixtureMemory(smp.Fixtures.Memory)
		if err != nil {
			return app.Deps{}, err
		}
		return app.Deps{Clock: s.Clock, Logger: trialLog, DB: s.DB, Redis: in.rdb, Keys: s.Keys,
			RawStore: repository.NewRawStore(s.DB, putter, cfg.MinIO.BucketRaw),
			// 主链路（桩模型）在 Build 内用本 trial 的 Runs / Advice / Drafts / RunLocker 装配；Tools 为每 trial 一个 FixtureMCP。
			NewAgent: app.GraphAgent(ctx, app.GraphConfig{Model: mcfg, Chat: chat, KB: ls.kb, Taxonomy: ls.tx}),
			Tools:    tools, Risk: risk, Adapter: &env.RecordingAdapter{},
			// L6：检查点读回走 MinIO；记忆按配置开关注入样本 fixtures.memory（不读 trial 库 episodic_memory）。
			HITL: app.HITLDeps{Objects: repository.MinIOGetter{Client: mc}, RawBucket: cfg.MinIO.BucketRaw,
				Memory: memory.Options{Enabled: cfg.Memory.Enabled, EpisodicLimit: cfg.Memory.EpisodicLimit,
					RuneBudget: cfg.Memory.RuneBudget}, MemorySource: mem}}, nil
	}
	return in, nil
}

func (in *infra) close() error {
	var errs []error
	if in.pool != nil {
		errs = append(errs, in.pool.Close())
	}
	if in.rdb != nil {
		if err := in.rdb.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close redis: %w", err))
		}
	}
	if in.evalDB != nil {
		errs = append(errs, repository.Close(in.evalDB))
	}
	return errors.Join(errs...)
}
