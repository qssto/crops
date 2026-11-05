package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// EvalReview 是专家评审接口连评测库的配置（P2-L2b，configs/app.yaml 的 eval_review 段）：
//   - Enabled：server / worker 是否连评测库（缺省 false；关闭时评审接口返回 503 EVAL_REVIEW_DISABLED，worker 不入队）；
//   - Host / Port / Database：评测库地址（库名缺省 agri_eval）；
//   - User / Password：独立账号，只从环境变量或 deploy/.env 取（AS_EVAL_REVIEW_USER / AS_EVAL_REVIEW_PASSWORD），
//     yaml 中出现即报错；不得与业务库账号 mysql.user 相同；
//   - MaxOpenConns：连接池上限；EnqueueInterval / EnqueueBatch：worker online-judge-review-enqueue 的周期与单农场每页行数。
type EvalReview struct {
	Enabled         bool          `mapstructure:"enabled"`
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	User            string        `mapstructure:"user"`     // secret
	Password        string        `mapstructure:"password"` // secret
	Database        string        `mapstructure:"database"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	EnqueueInterval time.Duration `mapstructure:"enqueue_interval"`
	EnqueueBatch    int           `mapstructure:"enqueue_batch"`
}

// 评审连接缺省值。
const (
	DefaultEvalReviewHost         = "127.0.0.1"
	DefaultEvalReviewPort         = 3306
	DefaultEvalReviewDatabase     = "agri_eval"
	DefaultEvalReviewMaxOpenConns = 4
	DefaultEvalReviewInterval     = time.Hour
	DefaultEvalReviewBatch        = 200
	// MaxEvalReviewBatch 是 enqueue_batch 上限：单页读业务库 / 单条多值 INSERT 评测库的行数，过大会长事务、大包。
	MaxEvalReviewBatch = 1000
	maxPort            = 65535
)

// DefaultEvalReview 是缺省配置（不启用、无账号）。
func DefaultEvalReview() EvalReview {
	return EvalReview{Host: DefaultEvalReviewHost, Port: DefaultEvalReviewPort, Database: DefaultEvalReviewDatabase,
		MaxOpenConns: DefaultEvalReviewMaxOpenConns, EnqueueInterval: DefaultEvalReviewInterval, EnqueueBatch: DefaultEvalReviewBatch}
}

func setEvalReviewDefaults(v *viper.Viper) {
	d := DefaultEvalReview()
	v.SetDefault("eval_review.enabled", false)
	v.SetDefault("eval_review.host", d.Host)
	v.SetDefault("eval_review.port", d.Port)
	v.SetDefault("eval_review.database", d.Database)
	v.SetDefault("eval_review.max_open_conns", d.MaxOpenConns)
	v.SetDefault("eval_review.enqueue_interval", d.EnqueueInterval)
	v.SetDefault("eval_review.enqueue_batch", d.EnqueueBatch)
}

// ErrBadEvalReview 表示 eval_review 配置非法。
var ErrBadEvalReview = errors.New("eval_review config invalid")

// Validate 只在启用时校验：地址 / 库名非空、端口合法、连接数 / 周期 > 0、批量在 [1, MaxEvalReviewBatch]，且账号不得与业务库账号 mysqlUser 相同
// （独立账号、最小授权）。账号与口令是否取到由启动时 RequireSecrets 校验。
func (e EvalReview) Validate(mysqlUser string) error {
	if !e.Enabled {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrBadEvalReview}, args...)...))
	}
	if e.Host == "" {
		bad("eval_review.host must be set")
	}
	if e.Port <= 0 || e.Port > maxPort {
		bad("eval_review.port %d out of range", e.Port)
	}
	if e.Database == "" {
		bad("eval_review.database must be set")
	}
	if e.MaxOpenConns <= 0 {
		bad("eval_review.max_open_conns must be > 0, got %d", e.MaxOpenConns)
	}
	if e.EnqueueInterval <= 0 {
		bad("eval_review.enqueue_interval must be > 0, got %s", e.EnqueueInterval)
	}
	if e.EnqueueBatch <= 0 || e.EnqueueBatch > MaxEvalReviewBatch {
		bad("eval_review.enqueue_batch must be in [1, %d], got %d", MaxEvalReviewBatch, e.EnqueueBatch)
	}
	if e.User != "" && e.User == mysqlUser {
		bad("eval_review.user must differ from mysql.user (separate least-privilege account)")
	}
	return errors.Join(errs...)
}

// DSN 返回评测库连接串（复用 MySQL.DSN：parseTime + loc=UTC，不开 multiStatements）。
func (e EvalReview) DSN() string {
	return MySQL{Host: e.Host, Port: e.Port, User: e.User, Password: e.Password}.DSN(e.Database, false)
}
