package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/service/quality"
	"agri-steward/internal/service/quality/judge"
)

// P2-L3：可信度日报（quality-metric-rollup + online-judge-sample + 日报）登记为 worker 任务 quality-daily。
func init() { registerP2Loops("P2-L3", buildQualityLoops) }

// LoopQualityDaily 是可信度日报任务名。
const LoopQualityDaily = "quality-daily"

// QualityDeps 是 P2-L3 依赖（boot 装配）；Config.Enabled 为 false 或 Objects 为空时 worker 不起任务（行为不变）。
type QualityDeps struct {
	Config config.Quality
	// Model 提供 judge 档位的生产 rate_per_min（rl:model:{tier} 与主链路共用）。
	Model *config.Model
	// Objects / Bucket 是 MinIO agri-steward-eval：读 nightly/latest.json 与 metrics.json，写日报。
	Objects quality.Objects
	Bucket  string
	// KB 打开 agent_run.knowledge_ref 对应的知识快照（judge 材料的引用条目）。
	KB *knowledge.Store
	// Repo 是仓库根目录：Config.JudgesDir 为相对路径时相对它解析。
	Repo string
}

// ErrQualityDeps 表示启用了日报但依赖缺失或档位未配置。
var ErrQualityDeps = errors.New("app: quality deps invalid")

func buildQualityLoops(a *App) ([]Loop, error) {
	d := a.quality
	if !d.Config.Enabled {
		return nil, nil
	}
	if d.Objects == nil || d.Bucket == "" || d.KB == nil || d.Model == nil {
		return nil, fmt.Errorf("%w: quality.enabled needs Objects, Bucket, KB and Model", ErrQualityDeps)
	}
	job, err := a.qualityJob(d)
	if err != nil {
		return nil, err
	}
	return []Loop{{Name: LoopQualityDaily, Run: job.Run}}, nil
}

func (a *App) qualityJob(d QualityDeps) (*quality.Job, error) {
	q := d.Config
	tier, ok := d.Model.Tiers[q.JudgeTier]
	if !ok {
		return nil, fmt.Errorf("%w: quality.judge_tier %q not in model tiers", ErrQualityDeps, q.JudgeTier)
	}
	dir := q.JudgesDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(d.Repo, dir)
	}
	prompt, err := judge.LoadPrompt(dir, judge.DimFactConsistency)
	if err != nil {
		return nil, fmt.Errorf("app: quality judge prompt: %w", err)
	}
	version, err := judge.Version(dir, judge.DimFactConsistency, q.JudgeTier)
	if err != nil {
		return nil, fmt.Errorf("app: quality judge version: %w", err)
	}
	// 待真实模型：现阶段 judge 只有录制回放（无录制的卡为 UNKNOWN，当日「不可判」）。
	recordings, err := judge.LoadRecordings(dir)
	if err != nil {
		return nil, fmt.Errorf("app: quality judge recordings: %w", err)
	}
	loc, err := time.LoadLocation(q.ReportTimezone)
	if err != nil {
		return nil, fmt.Errorf("app: quality report timezone: %w", err)
	}
	store := repository.QualityRepo{DB: a.DB}
	sampler := &quality.Sampler{Store: store, Cards: a.Advice, KB: kbOpener{kb: d.KB}, Judge: recordings,
		Prompt: prompt, Version: version, Tokens: repository.NewTokenBucket(a.Redis),
		ModelKey: a.Keys.Raw("rl:model:" + q.JudgeTier), JudgeKey: a.Keys.RlJudge(q.JudgeTier),
		ModelQPM: tier.RatePerMin, JudgeQPM: q.JudgeQPM, Quota: q.SamplePerCategory, Salt: q.SampleSalt,
		Clock: a.Clock, Logger: a.Logger}
	job, err := quality.NewJob(quality.JobDeps{Farms: store, Rollup: quality.Rollup{Store: store, Version: version},
		Sampler: sampler, Reports: store, Objects: d.Objects, Bucket: d.Bucket,
		Notifier: &quality.RecordingNotifier{Logger: a.Logger}, // 待运行：钉钉通道接入前只记日志
		Locks:    a.Locker, Keys: a.Keys, Clock: a.Clock, Logger: a.Logger, Interval: q.Interval, ReportLoc: loc,
		BaselineName: q.BaselineName, BaselineID: q.BaselineExperimentID})
	if err != nil {
		return nil, fmt.Errorf("app: quality job: %w", err)
	}
	return job, nil
}

// kbOpener 把 knowledge.Store 适配为 quality.KnowledgeOpener（*Snapshot 实现 judge.EntryLoader）。
type kbOpener struct{ kb *knowledge.Store }

func (o kbOpener) Open(ctx context.Context, ref string) (judge.EntryLoader, error) {
	snap, err := o.kb.Open(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("app: open knowledge %s: %w", ref, err)
	}
	return snap, nil
}
