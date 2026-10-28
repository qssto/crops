package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/repository"
	"agri-steward/internal/seed"
)

// infraFlags 是 run 连接真实 MySQL / Redis / MinIO 的参数。
type infraFlags struct {
	configFile string
	envFile    string
	migrations string
	seedFile   string
	poolSize   int
}

func (f *infraFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.configFile, "config", "configs/app.yaml", "配置文件")
	fs.StringVar(&f.envFile, "env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
	fs.StringVar(&f.migrations, "migrations", "migrations", "trial schema 使用的业务库迁移目录")
	fs.StringVar(&f.seedFile, "seed", "configs/seed/fields.yaml", "每个 trial 重灌的种子文件")
	fs.IntVar(&f.poolSize, "pool", 1, "trial schema 槽位数（agri_eval_t_1..n）")
}

// infra 是一次评测持有的外部资源。
type infra struct {
	evalDB *repository.DB
	rdb    *redis.Client
	pool   *env.Pool
	deps   evalkit.DepsFunc
}

// open 依次连 agri_eval、Redis、trial 池；任一不可用都返回错误（不静默跳过）。
func (f *infraFlags) open(ctx context.Context, logW io.Writer) (_ *infra, err error) {
	cfg, err := config.Load(f.configFile, f.envFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireSecrets("mysql.password", "redis.password", "minio.access_key", "minio.secret_key"); err != nil {
		return nil, err
	}
	data, err := seed.Load(f.seedFile)
	if err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(logW, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	in := &infra{}
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
	in.deps = func(s *env.Slot) app.Deps {
		return app.Deps{Clock: s.Clock, Logger: logger, DB: s.DB, Redis: in.rdb, Keys: s.Keys,
			RawStore: repository.NewRawStore(s.DB, putter, cfg.MinIO.BucketRaw),
			// L1 注入只拒答的空实现桩；L4 起换成真实主链路 + FixtureMCP 等评测接缝。
			Agent: evalkit.RefuseAllAgent{}}
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
