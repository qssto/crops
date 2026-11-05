package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P2-L2b 装配：专家评审（server 平台路由 /api/v1/admin/review-tasks）与线上 judge 专家队列入队
// online-judge-review-enqueue（worker 任务）。两者都只在 eval_review.enabled 时连评测库；关闭时路由照挂、
// 鉴权照做、之后 503 EVAL_REVIEW_DISABLED，worker 不起入队任务。评测库 review_task 是平台表（无 tenant / farm），
// SystemScope 只在本文件构造（archtest systemScopeCallers 白名单一行）。

// 系统凭证（入队任务名即 lock:p2job 的 job 名）。
var (
	reviewAPIScope     = repository.NewSystemScope("review-api")
	reviewEnqueueScope = repository.NewSystemScope(service.ReviewEnqueueJob)
)

// EvalReviewDeps 是评审依赖（boot 装配）：Config.Enabled 为 false 时 DB 必须为空，评审服务为 DisabledReviewService。
type EvalReviewDeps struct {
	Config config.EvalReview
	// DB 是评测库连接（独立账号，最小授权：review_task SELECT / INSERT / UPDATE、trial SELECT）；由 boot 打开与关闭。
	DB *gorm.DB
	// Objects / Bucket 是 MinIO agri-steward-eval：BACKFLOW_EXPECT 结论写 review/backflow_expect/{draft_ref}.json。
	Objects service.ReviewObjects
	Bucket  string
}

// ErrEvalReviewDeps 表示 eval_review 的开关与依赖不一致（启用缺依赖，或未启用却传了连接）。
var ErrEvalReviewDeps = errors.New("app: eval_review deps invalid")

func init() { registerP2Loops("P2-L2b", reviewLoops) }

// OpenEvalReview 在 eval_review.enabled 时用独立账号打开评测库连接（账号口令只从环境变量 / deploy/.env 取，缺失即失败）；
// 未启用返回 nil, nil（不连库）。调用方负责 repository.Close。
func OpenEvalReview(ctx context.Context, cfg *config.Config, c clock.Clock, log *slog.Logger) (*gorm.DB, error) {
	if !cfg.EvalReview.Enabled {
		return nil, nil
	}
	if err := cfg.RequireSecrets("eval_review.user", "eval_review.password"); err != nil {
		return nil, err
	}
	db, err := repository.OpenMySQL(ctx, repository.OpenOptions{DSN: cfg.EvalReview.DSN(),
		MaxOpenConns: cfg.EvalReview.MaxOpenConns, Clock: c, Logger: log})
	if err != nil {
		return nil, fmt.Errorf("app: open eval review db %s:%d/%s: %w", cfg.EvalReview.Host, cfg.EvalReview.Port,
			cfg.EvalReview.Database, err)
	}
	return db, nil
}

// buildReview 装配评审服务：未启用 → DisabledReviewService；启用缺依赖 → 启动失败（不静默降级成关闭）。
func (a *App) buildReview() error {
	d := a.evalReview
	if !d.Config.Enabled {
		if d.DB != nil {
			return fmt.Errorf("%w: eval_review.enabled=false but an eval DB was passed", ErrEvalReviewDeps)
		}
		a.Review = service.DisabledReviewService()
		return nil
	}
	if d.DB == nil || d.Objects == nil || d.Bucket == "" {
		return fmt.Errorf("%w: eval_review.enabled needs DB, Objects and Bucket", ErrEvalReviewDeps)
	}
	svc, err := service.NewReviewService(service.ReviewDeps{Store: repository.EvalReviewRepo{DB: d.DB},
		Judge: repository.QualityRepo{DB: a.DB}, Audit: repository.AuditRepo{DB: a.DB}, Objects: d.Objects,
		Bucket: d.Bucket, Mask: pii.Mask, Clock: a.Clock, System: reviewAPIScope})
	if err != nil {
		return fmt.Errorf("app: build review: %w", err)
	}
	a.Review = svc
	return nil
}

// reviewAdminMounts 是平台路由 /api/v1/admin/review-tasks（review.read / review.submit，只授 kb_expert）。
func (a *App) reviewAdminMounts(g controller.Guard) []controller.EngineMount {
	if a.Review == nil {
		return nil
	}
	return []controller.EngineMount{controller.ReviewAdminRoutes(g, a.Review)}
}

// reviewLoops：未启用不起任务；启用但依赖缺失时 worker 启动失败；齐全起 online-judge-review-enqueue。
func reviewLoops(a *App) ([]Loop, error) {
	if a == nil || !a.evalReview.Config.Enabled {
		return nil, nil
	}
	d := a.evalReview
	if a.DB == nil || a.Locker == nil || a.Redis == nil || d.DB == nil {
		return nil, fmt.Errorf("%w: online judge review enqueue needs DB, eval DB, Redis and Locker", ErrEvalReviewDeps)
	}
	job, err := service.NewReviewEnqueue(service.ReviewEnqueueDeps{Source: repository.QualityRepo{DB: a.DB},
		Sink: repository.EvalReviewRepo{DB: d.DB}, Cursor: repository.NewReviewEnqueueCursor(a.Redis, a.Keys),
		Locks: a.Locker, LockKey: a.Keys.LockP2Job(service.ReviewEnqueueJob),
		Clock: a.Clock, Logger: a.Logger, System: reviewEnqueueScope, Interval: d.Config.EnqueueInterval,
		Batch: d.Config.EnqueueBatch})
	if err != nil {
		return nil, fmt.Errorf("app: online judge review enqueue: %w", err)
	}
	return []Loop{{Name: service.ReviewEnqueueJob, Run: job.Run}}, nil
}
