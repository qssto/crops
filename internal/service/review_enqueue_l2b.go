package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// P2-L2b 线上 judge 专家队列入队（worker 任务 online-judge-review-enqueue，只在 eval_review.enabled 时起）：
// 逐农场按 id 分页读 online_judge_result 的 NEED_EXPERT 行，写评测库 review_task(ONLINE_JUDGE)，draft_ref 记
// ojr:{tenant}:{farm}:{id}，按 ref_key 去重（重跑不重复派单）。单实例互斥走 lock:p2job:{job}，每农场续期一次。
// 单农场单轮至多读 reviewEnqueueMaxPagesPerFarm 页：读满时把最后一个 id 记进 Redis 游标
// cursor:review_enqueue:{tenant}:{farm}，下一轮从游标续扫；扫到末尾删除游标，下一轮从头扫（重读已入队行，靠去重跳过）。

// ReviewEnqueueJob 是任务名（Loop 名与 lock:p2job:{job} 的 job）。
const ReviewEnqueueJob = "online-judge-review-enqueue"

// 入队缺省值。
const (
	DefaultReviewEnqueueInterval = time.Hour
	DefaultReviewEnqueueBatch    = 200
	reviewEnqueueMaxPagesPerFarm = 50 // 单农场单轮至多读这么多页，余量由游标留到下一轮续扫，避免单轮长时间持锁
)

// ReviewEnqueueSource 是业务库侧来源（repository.QualityRepo 实现）。
type ReviewEnqueueSource interface {
	FarmTimezones(ctx context.Context, sys repository.SystemScope) ([]repository.FarmTZ, error)
	NeedExpertResults(ctx context.Context, scope model.Scope, afterID uint64, limit int) ([]model.OnlineJudgeResult, error)
}

// ReviewEnqueueSink 是评测库侧去重插入（repository.EvalReviewRepo 实现）。
type ReviewEnqueueSink interface {
	InsertReviewTasks(ctx context.Context, sys repository.SystemScope, tasks []model.ReviewTask, at time.Time) (int, error)
}

// ReviewEnqueueCursorStore 是逐农场续扫游标（repository.ReviewEnqueueCursor 实现；不存在返回 0）。
type ReviewEnqueueCursorStore interface {
	EnqueueCursor(ctx context.Context, scope model.Scope) (uint64, error)
	SetEnqueueCursor(ctx context.Context, scope model.Scope, after uint64) error
	ResetEnqueueCursor(ctx context.Context, scope model.Scope) error
}

// ReviewEnqueueDeps 是入队任务依赖；System 由 app 构造（archtest SystemScope 白名单）。
type ReviewEnqueueDeps struct {
	Source   ReviewEnqueueSource
	Sink     ReviewEnqueueSink
	Cursor   ReviewEnqueueCursorStore // 可空：每轮从头扫（单测；app 必配）
	Locks    KeyLocker                // 可空：不互斥（单测）
	LockKey  string                   // Keys.LockP2Job(ReviewEnqueueJob)
	Clock    clock.Clock
	Logger   *slog.Logger
	System   repository.SystemScope
	Interval time.Duration // ≤ 0 取 DefaultReviewEnqueueInterval
	Batch    int           // ≤ 0 取 DefaultReviewEnqueueBatch
}

// ReviewEnqueue 是入队任务。
type ReviewEnqueue struct{ d ReviewEnqueueDeps }

// NewReviewEnqueue 校验依赖并补缺省。
func NewReviewEnqueue(d ReviewEnqueueDeps) (*ReviewEnqueue, error) {
	if d.Source == nil || d.Sink == nil || d.Clock == nil || d.Logger == nil {
		return nil, errors.New("service: review enqueue needs Source, Sink, Clock and Logger")
	}
	if d.Interval <= 0 {
		d.Interval = DefaultReviewEnqueueInterval
	}
	if d.Batch <= 0 {
		d.Batch = DefaultReviewEnqueueBatch
	}
	return &ReviewEnqueue{d: d}, nil
}

// Run 启动即跑一轮，之后每个周期一轮，直到 ctx 取消。
func (j *ReviewEnqueue) Run(ctx context.Context) {
	t := time.NewTicker(j.d.Interval)
	defer t.Stop()
	for {
		if _, err := j.Tick(ctx); err != nil && ctx.Err() == nil {
			j.d.Logger.WarnContext(ctx, "review enqueue", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 执行一轮，返回新入队任务数。锁被他人持有时返回 0, nil；续期失败（锁丢失）即停止并返回错误。
func (j *ReviewEnqueue) Tick(ctx context.Context) (int, error) {
	var lease agent.Lease
	if j.d.Locks != nil {
		l, err := j.d.Locks.AcquireLease(ctx, j.d.LockKey, rediskey.LockP2JobTTL)
		if errors.Is(err, repository.ErrLockHeld) {
			j.d.Logger.InfoContext(ctx, "p2 job lock held, skip", "job", ReviewEnqueueJob)
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("service: review enqueue lock: %w", err)
		}
		lease = l
		defer func() {
			if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
				j.d.Logger.WarnContext(ctx, "review enqueue release lock", "err", err)
			}
		}()
	}
	farms, err := j.d.Source.FarmTimezones(ctx, j.d.System)
	if err != nil {
		return 0, fmt.Errorf("service: review enqueue list farms: %w", err)
	}
	total := 0
	for _, f := range farms {
		n, err := j.farm(ctx, model.Scope{TenantID: f.TenantID, FarmID: f.FarmID})
		total += n
		if err != nil {
			return total, err
		}
		if lease != nil {
			if err := lease.Renew(ctx); err != nil {
				return total, fmt.Errorf("service: review enqueue renew lock: %w", err)
			}
		}
	}
	if total > 0 {
		j.d.Logger.InfoContext(ctx, "online judge review tasks enqueued", "tasks", total, "farms", len(farms))
	}
	return total, nil
}

// farm 从游标处按 id 分页入队一个农场的 NEED_EXPERT 行：扫到末尾删游标，读满页数上限记游标。
// 插入失败不推进游标（下一轮从上次记下的位置重读，去重保证不重复派单）。
func (j *ReviewEnqueue) farm(ctx context.Context, scope model.Scope) (int, error) {
	after, err := j.cursor(ctx, scope)
	if err != nil {
		return 0, err
	}
	total := 0
	now := j.d.Clock.Now()
	for range reviewEnqueueMaxPagesPerFarm {
		rows, err := j.d.Source.NeedExpertResults(ctx, scope, after, j.d.Batch)
		if err != nil {
			return total, fmt.Errorf("service: review enqueue farm %d: %w", scope.FarmID, err)
		}
		if len(rows) == 0 {
			return total, j.resetCursor(ctx, scope)
		}
		tasks := make([]model.ReviewTask, 0, len(rows))
		for _, r := range rows {
			tasks = append(tasks, model.ReviewTask{TaskType: model.ReviewOnlineJudge,
				DraftRef: model.OnlineJudgeRef(scope.TenantID, scope.FarmID, r.ID), Dimension: r.Dimension,
				JudgeVersion: r.JudgeVersion, JudgeScore: r.JudgeScore, RaterNo: model.RaterPrimary})
		}
		n, err := j.d.Sink.InsertReviewTasks(ctx, j.d.System, tasks, now)
		total += n
		if err != nil {
			return total, fmt.Errorf("service: review enqueue farm %d insert: %w", scope.FarmID, err)
		}
		after = rows[len(rows)-1].ID
		if len(rows) < j.d.Batch {
			return total, j.resetCursor(ctx, scope)
		}
	}
	if j.d.Cursor == nil {
		return total, nil
	}
	if err := j.d.Cursor.SetEnqueueCursor(ctx, scope, after); err != nil {
		return total, fmt.Errorf("service: review enqueue farm %d save cursor: %w", scope.FarmID, err)
	}
	return total, nil
}

// cursor 读农场续扫位置；未配游标恒为 0。
func (j *ReviewEnqueue) cursor(ctx context.Context, scope model.Scope) (uint64, error) {
	if j.d.Cursor == nil {
		return 0, nil
	}
	after, err := j.d.Cursor.EnqueueCursor(ctx, scope)
	if err != nil {
		return 0, fmt.Errorf("service: review enqueue farm %d load cursor: %w", scope.FarmID, err)
	}
	return after, nil
}

// resetCursor 扫到末尾时删游标，下一轮从头扫。
func (j *ReviewEnqueue) resetCursor(ctx context.Context, scope model.Scope) error {
	if j.d.Cursor == nil {
		return nil
	}
	if err := j.d.Cursor.ResetEnqueueCursor(ctx, scope); err != nil {
		return fmt.Errorf("service: review enqueue farm %d reset cursor: %w", scope.FarmID, err)
	}
	return nil
}
