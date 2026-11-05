package app

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
	"agri-steward/internal/service/attribution"
	"agri-steward/internal/service/backflow"
)

// P2 L8 装配：归因周报 attribution-weekly 与线上回流 backflow-collect（worker 任务）、问题处置 PATCH（server 平台路由）。
// 平台行 attribution_issue 的 SystemScope 只在本文件构造（archtest systemScopeCallers 白名单一行）。

// 系统扫描凭证（任务名即 lock:p2job 的 job 名）。
var (
	attributionWeeklyScope = repository.NewSystemScope(attribution.JobWeekly)
	attributionPatchScope  = repository.NewSystemScope("attribution-patch")
	backflowCollectScope   = repository.NewSystemScope(backflow.JobCollect)
)

// AttributionDeps 是 P2 L8 归因依赖（boot 装配）：Config.Enabled 为 false 时 worker 不起周报任务。
type AttributionDeps struct {
	Config config.Attribution
	// Objects / Bucket 是 MinIO agri-steward-eval：写 report/weekly/{week}.html，读 nightly/kb_load_gaps.json。
	Objects attribution.Objects
	Bucket  string
}

// ErrAttributionDeps 表示启用了周报但依赖缺失。
var ErrAttributionDeps = errors.New("app: attribution deps invalid")

func init() { registerP2Loops("P2-L8", attributionLoops) }

// attributionLoops：周报与回流各自开关；都未启用不起任务；启用但依赖缺失或配置非法时 worker 启动失败。
func attributionLoops(a *App) ([]Loop, error) {
	if a == nil {
		return nil, nil
	}
	cfg := a.attribution.Config
	if !cfg.Enabled && !cfg.Backflow.Enabled {
		return nil, nil
	}
	if a.DB == nil || a.Locker == nil {
		return nil, fmt.Errorf("%w: attribution / backflow needs DB and Locker", ErrAttributionDeps)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}
	var loops []Loop
	if cfg.Enabled {
		l, err := a.attributionWeeklyLoop(loc)
		if err != nil {
			return nil, err
		}
		loops = append(loops, l)
	}
	if cfg.Backflow.Enabled {
		l, err := a.backflowLoop(loc)
		if err != nil {
			return nil, err
		}
		loops = append(loops, l)
	}
	return loops, nil
}

// attributionWeeklyLoop 是 attribution-weekly（需 eval 桶读写）。
func (a *App) attributionWeeklyLoop(loc *time.Location) (Loop, error) {
	d := a.attribution
	if d.Objects == nil || d.Bucket == "" {
		return Loop{}, fmt.Errorf("%w: attribution.enabled needs Objects and Bucket", ErrAttributionDeps)
	}
	repo := repository.AttributionRepo{DB: a.DB}
	// Experts 不接：业务 worker 不连评测库（review_task 来源待调度方决定 server 连评测库）。
	job, err := attribution.NewJob(attribution.JobDeps{Farms: repository.QualityRepo{DB: a.DB}, Hits: repo, Issues: repo,
		Objects: d.Objects, Bucket: d.Bucket, Locks: a.Locker, Keys: a.Keys, Clock: a.Clock, Logger: a.Logger,
		System: attributionWeeklyScope, Loc: loc, TopN: d.Config.TopN, Interval: d.Config.Interval,
		KBLoadGapsKey: d.Config.KBLoadGapsKey})
	if err != nil {
		return Loop{}, fmt.Errorf("app: attribution weekly: %w", err)
	}
	return Loop{Name: attribution.JobWeekly, Run: job.Run}, nil
}

// backflowLoop 是 backflow-collect：原文只经 a.Audit.ReadRaw 读（每条写 RAW_READ 审计），故需装配了原文读取（Deps.Presign）。
// 不连评测库：review_task(BACKFLOW_EXPECT) 由 cmd/eval backflow import 读草稿后建。
func (a *App) backflowLoop(loc *time.Location) (Loop, error) {
	if a.Audit == nil {
		return Loop{}, fmt.Errorf("%w: attribution.backflow.enabled needs raw read (Deps.Presign)", ErrAttributionDeps)
	}
	b := a.attribution.Config.Backflow
	job, err := backflow.NewJob(backflow.JobDeps{Farms: repository.QualityRepo{DB: a.DB},
		Hits: repository.AttributionRepo{DB: a.DB}, Store: repository.BackflowRepo{DB: a.DB}, Raw: a.Audit,
		Fetch: repository.PresignedFetcher{Client: &http.Client{Timeout: backflowFetchTimeout}}, Locks: a.Locker,
		Keys: a.Keys, Clock: a.Clock, Logger: a.Logger, System: backflowCollectScope, Loc: loc,
		Interval: a.attribution.Config.Interval, DraftsDir: b.DraftsDir, KBMissMinHits: b.KBMissMinHits,
		MaxRuns: b.MaxRuns, ServiceUserID: b.ServiceUserID})
	if err != nil {
		return Loop{}, fmt.Errorf("app: backflow collect: %w", err)
	}
	return Loop{Name: backflow.JobCollect, Run: job.Run}, nil
}

// backflowFetchTimeout 是取一条预签名原文的超时（预签名 5 分钟内有效）。
const backflowFetchTimeout = 30 * time.Second

// buildAttributionIssues 装配处置服务（只依赖业务库与时钟，总是装配；路由权限 attribution.write 兜底）。
func (a *App) buildAttributionIssues() (*service.AttributionIssueService, error) {
	s, err := service.NewAttributionIssueService(service.AttributionIssueDeps{Store: repository.AttributionRepo{DB: a.DB},
		Clock: a.Clock, System: attributionPatchScope})
	if err != nil {
		return nil, fmt.Errorf("app: build attribution issues: %w", err)
	}
	return s, nil
}

// l8AdminMounts 是平台路由 PATCH /api/v1/admin/attribution-issues/:id。
func (a *App) l8AdminMounts(g controller.Guard) []controller.EngineMount {
	if a.L8 == nil || a.L8.Issues == nil {
		return nil
	}
	return []controller.EngineMount{controller.AttributionAdminRoutes(g, a.L8.Issues)}
}
