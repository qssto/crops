// Package attribution 是 P2 L8 归因周报 attribution-weekly（§6.9 技术实现 2）：每周一（注入时钟、配置时区）取上一 ISO 周
// 全部农场的负向信号，按「原因 × 问题类型 × 主 kb_id」聚合成平台行 attribution_issue（跨周一签名一行，已关闭再命中即重开），
// 写 eval 桶 report/weekly/{week}.html；done 标记 report/weekly/{week}.json 兼作下周「上期处置回填」的快照。
package attribution

import (
	"context"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 仓储能力按用途拆开，repository.QualityRepo（农场列表）与 repository.AttributionRepo（其余）实现。

// FarmLister 列出全部农场（系统级）。
type FarmLister interface {
	FarmTimezones(ctx context.Context, sys repository.SystemScope) ([]repository.FarmTZ, error)
}

// HitStore 是单农场取上周负向信号与回流计数的能力（农场 Scope）。
type HitStore interface {
	WeekHits(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.AttributionHit, error)
	BackflowCounts(ctx context.Context, scope model.Scope, from, to time.Time) (map[string]int, error)
}

// IssueStore 是平台问题行的读写（系统级）。
type IssueStore interface {
	IssuesBySignature(ctx context.Context, sys repository.SystemScope, sigs []string) ([]model.AttributionIssue, error)
	WeekIssues(ctx context.Context, sys repository.SystemScope, week string, limit int) ([]model.AttributionIssue, error)
	UpsertIssues(ctx context.Context, sys repository.SystemScope, rows []model.IssueUpsert, at time.Time) error
}

// ExpertReviewSource 是评测库 review_task 的专家判错来源。业务 worker 不连评测库：待调度方决定 server 连评测库，
// 本期不接（nil），周报注明该来源未接入。
type ExpertReviewSource interface {
	WeekHits(ctx context.Context, from, to time.Time) ([]model.AttributionHit, error)
}

// Objects 读写 eval 桶对象（repository.QualityObjects 实现）；对象不存在时 GetObject 返回 model.ErrNotFound。
type Objects interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	PutObject(ctx context.Context, bucket, key string, body []byte, contentType string) error
}

// Locker 取 Redis token 锁；被持有返回 repository.ErrLockHeld（repository.Locker 实现）。
type Locker interface {
	AcquireLease(ctx context.Context, key string, ttl time.Duration) (agent.Lease, error)
}
