package main

import (
	"context"
	"flag"
	"io"

	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// storeFlags 是 baseline / compare / aa 只读写评测库与 MinIO 时的连接参数（不装配被测 agent）。
type storeFlags struct {
	configFile string
	envFile    string
}

func (f *storeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.configFile, "config", "configs/app.yaml", "配置文件")
	fs.StringVar(&f.envFile, "env-file", "deploy/.env", "密钥文件（不存在则只读环境变量）")
}

// evalStore 是评测库（mysql.eval_database）与 eval bucket 的连接。
type evalStore struct {
	db      *repository.DB
	exps    *store.Experiments
	objects repository.ObjectPutter
	// getter 读 eval bucket（pointer-only 校验登记名）。
	getter repository.ObjectGetter
	bucket string
}

// open 连评测库与 MinIO；密钥只从 env-file / 环境变量取。
func (f *storeFlags) open(ctx context.Context, logW io.Writer) (*evalStore, error) {
	cfg, err := config.Load(f.configFile, f.envFile)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireSecrets("mysql.password", "minio.access_key", "minio.secret_key"); err != nil {
		return nil, err
	}
	logger, err := app.NewLogger(logW, cfg.Log.Level)
	if err != nil {
		return nil, err
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		return nil, err
	}
	db, err := app.OpenMySQL(ctx, cfg, cfg.MySQL.EvalDatabase, clock.System{}, logger)
	if err != nil {
		return nil, err
	}
	return &evalStore{db: db, exps: store.New(db), objects: repository.MinIOPutter{Client: mc},
		getter: repository.MinIOGetter{Client: mc}, bucket: cfg.MinIO.BucketEval}, nil
}

func (s *evalStore) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return repository.Close(s.db)
}
