package quality

import (
	"context"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service/quality/judge"
)

// 仓储能力按用途拆开（rollup / 抽样 / 日报 / 专家回写），repository.QualityRepo 全部实现。

// FarmLister 列出全部农场及时区（系统级）。
type FarmLister interface {
	FarmTimezones(ctx context.Context, sys repository.SystemScope) ([]repository.FarmTZ, error)
}

// RollupStore 是 rollup 读 advice_card / online_judge_result、写 quality_metric_daily 的能力。
type RollupStore interface {
	AdviceOutcomeCounts(ctx context.Context, scope model.Scope, from, to time.Time) ([]repository.OutcomeCount, error)
	CitedAdviceCount(ctx context.Context, scope model.Scope, from, to time.Time) (int, error)
	FarmJudgeTally(ctx context.Context, scope model.Scope, statDate, dimension, version string, top int) (repository.JudgeTally, error)
	PlatformJudgeTally(ctx context.Context, sys repository.SystemScope, statDate, dimension, version string, top int) (repository.JudgeTally, error)
	OnlineFarmTotals(ctx context.Context, sys repository.SystemScope, statDate string) ([]repository.MetricTotal, error)
	UpsertDaily(ctx context.Context, sys repository.SystemScope, rows []model.QualityDaily, at time.Time) error
}

// SampleStore 是线上 judge 抽样的能力。
type SampleStore interface {
	AdviceCategories(ctx context.Context, scope model.Scope, from, to time.Time) ([]string, error)
	SampleAdvice(ctx context.Context, scope model.Scope, category string, from, to time.Time, salt string, limit int) ([]repository.AdviceRef, error)
	JudgeResults(ctx context.Context, scope model.Scope, statDate, dimension string) ([]model.OnlineJudgeResult, error)
	InsertJudgeResult(ctx context.Context, scope model.Scope, j model.OnlineJudgeResult, at time.Time) (bool, error)
	RunKnowledgeRef(ctx context.Context, scope model.Scope, runID uint64) (string, error)
}

// ReportStore 是日报读平台行与下钻 run 的能力。
type ReportStore interface {
	DailyRows(ctx context.Context, sys repository.SystemScope, tenantID, farmID uint64, statDate string) ([]model.QualityDaily, error)
	AdviceRuns(ctx context.Context, scope model.Scope, from, to time.Time, f repository.RunFilter, limit int) ([]uint64, error)
	JudgeResults(ctx context.Context, scope model.Scope, statDate, dimension string) ([]model.OnlineJudgeResult, error)
}

// ReviewStore 是专家回写的能力。
type ReviewStore interface {
	ReviewJudgeResult(ctx context.Context, scope model.Scope, id uint64, rowVer, score int, expertID uint64, at time.Time) error
}

// CardReader 按 run_id 读建议卡（repository.AdviceRepo 实现）。
type CardReader interface {
	GetByRun(ctx context.Context, scope model.Scope, runID uint64) (model.AdviceCard, error)
}

// KnowledgeOpener 按 knowledge_ref 打开知识版本（app 用 knowledge.Store 适配）。
type KnowledgeOpener interface {
	Open(ctx context.Context, ref string) (judge.EntryLoader, error)
}

// TokenTaker 从令牌桶取一个令牌、或退还取到但没用上的令牌（repository.TokenBucket 实现）；perMinute <= 0 不限。
type TokenTaker interface {
	Take(ctx context.Context, key string, perMinute int, now time.Time) (bool, error)
	Refund(ctx context.Context, key string, perMinute int, now time.Time) error
}

// Objects 读写 eval 桶对象（repository.QualityObjects 实现）；对象不存在时 GetObject 返回 model.ErrNotFound。
type Objects interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error
}
