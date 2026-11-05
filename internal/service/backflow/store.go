// Package backflow 是 P2 L8 线上回流 backflow-collect（§6.9 技术实现 4）：每周选出差评、专家判错、护栏拦截、
// 高频 KB_MISS 拒答的 run 置 SELECTED，以服务账号经 service.AuditService.ReadRaw（每次写 RAW_READ 审计）取工具返回原文，
// 连同 audit_log 的提问时刻、knowledge_ref 生成样本 fixture 草稿；脱敏后过 pii_clean，通过写草稿目录 → DRAFTED，
// 不过 → PII_FAILED。真实 ID 与草稿的对应只写业务库 agent_run.backflow_draft_ref，草稿内只有随机合成码。
// review_task(BACKFLOW_EXPECT) 不由业务 worker 写（业务 worker 不连评测库），由 cmd/eval backflow import 读草稿建。
package backflow

import (
	"context"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// FarmLister 列出全部农场与时区（系统级，repository.QualityRepo 实现）。
type FarmLister interface {
	FarmTimezones(ctx context.Context, sys repository.SystemScope) ([]repository.FarmTZ, error)
}

// HitStore 取单农场一周的负向信号（与归因周报同一口径，repository.AttributionRepo 实现）。
type HitStore interface {
	WeekHits(ctx context.Context, scope model.Scope, from, to time.Time) ([]model.AttributionHit, error)
}

// Store 是 agent_run 回流状态与草稿材料（农场 Scope，repository.BackflowRepo 实现）。
type Store interface {
	MarkSelected(ctx context.Context, scope model.Scope, runIDs []uint64, at time.Time) (int64, error)
	SelectedRuns(ctx context.Context, scope model.Scope, limit int) ([]uint64, error)
	Material(ctx context.Context, scope model.Scope, runID uint64) (model.BackflowMaterial, error)
	FinishDraft(ctx context.Context, scope model.Scope, runID uint64, status, draftRef string, at time.Time) error
	// FailAttempt 记一次失败的出草稿尝试；累计达 maxAttempts 次时把 run 置 FAILED 并返回 failed=true。
	FailAttempt(ctx context.Context, scope model.Scope, runID uint64, maxAttempts int, at time.Time) (failed bool, err error)
}

// RawReader 是带审计的原文读取（*service.AuditService 实现：每次调用写一条 RAW_READ）。
type RawReader interface {
	ReadRaw(ctx context.Context, scope model.Scope, runID, rawID uint64) (service.RawLink, error)
}

// Fetcher 按 ReadRaw 签发的预签名 URL 取原文（repository.PresignedFetcher 实现）。
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// Locker 取 Redis token 锁；被持有返回 repository.ErrLockHeld（repository.Locker 实现）。
type Locker interface {
	AcquireLease(ctx context.Context, key string, ttl time.Duration) (agent.Lease, error)
}

// ExpertTransition 是 DRAFTED → MERGED / DROPPED 的推进入口：由专家补写 expect、署名入库（或判定不入库）的流程触发。
// 待专家：本期只留接口，不实现、不装配。
type ExpertTransition interface {
	Merge(ctx context.Context, draftRef string, at time.Time) error
	Drop(ctx context.Context, draftRef string, reason string, at time.Time) error
}
