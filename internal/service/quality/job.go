package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service/quality/judge"
)

// 每日任务（§6.4 技术实现 2-4）：online-judge-sample（lock:p2job:online-judge-sample）→ quality-metric-rollup
// （lock:p2job:quality-metric-rollup：农场行、离线导入、平台行）→ 日报 HTML → 推送摘要。两段各自互斥，防止手动补跑与定时任务并发。
const (
	JobSample       = "online-judge-sample"
	JobRollup       = "quality-metric-rollup"
	drillRunsPerRow = 5 // 日报每个线上指标列出的下钻 run 数
)

// qualitySystem 是 rollup 跨农场读写的系统级凭证（archtest systemScopeCallers 白名单含本文件）。
var qualitySystem = repository.NewSystemScope(JobRollup)

// Locker 取 Redis token 锁；被持有返回 repository.ErrLockHeld（repository.Locker 实现）。
type Locker interface {
	AcquireLease(ctx context.Context, key string, ttl time.Duration) (agent.Lease, error)
}

// JobDeps 是每日任务的依赖；Farms / Rollup / Reports / Objects / Locks / Clock 必填，Sampler 为 nil 时当日事实一致率「未评」。
type JobDeps struct {
	Farms        FarmLister
	Rollup       Rollup
	Sampler      *Sampler
	Reports      ReportStore
	Objects      Objects
	Bucket       string
	Notifier     Notifier
	Locks        Locker
	Keys         rediskey.Keys
	Clock        clock.Clock
	Logger       *slog.Logger
	Interval     time.Duration
	ReportLoc    *time.Location // 日报业务日的时区
	BaselineName string
	BaselineID   uint64 // baseline_p1.r2 的 experiment_id；0 表示未配置（差值「—」）
}

// Job 是每日质量任务。「当日已完成」不放进程内存，而看 eval 桶里的推送标记（model.DailyNotifiedKey）：重启、多实例都只推一次。
type Job struct {
	d JobDeps
}

// NewJob 校验依赖。
func NewJob(d JobDeps) (*Job, error) {
	if d.Farms == nil || d.Rollup.Store == nil || d.Reports == nil || d.Objects == nil || d.Locks == nil || d.Clock == nil {
		return nil, errors.New("quality: job needs Farms, Rollup.Store, Reports, Objects, Locks and Clock")
	}
	if d.ReportLoc == nil {
		return nil, errors.New("quality: job needs ReportLoc")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Notifier == nil {
		d.Notifier = &RecordingNotifier{Logger: d.Logger}
	}
	if d.Interval <= 0 {
		d.Interval = time.Hour
	}
	return &Job{d: d}, nil
}

// Run 启动即检查一次，之后每个周期检查，直到 ctx 取消。
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.d.Interval)
	defer t.Stop()
	for {
		if err := j.Tick(ctx); err != nil {
			j.d.Logger.WarnContext(ctx, "quality daily job", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 在日报日（report_timezone 下的前一日）尚未推送时执行 RunOnce；已有推送标记即跳过（持久判定）。
func (j *Job) Tick(ctx context.Context) error {
	date := PrevDay(j.d.Clock.Now(), j.d.ReportLoc).Date
	done, err := j.notified(ctx, date)
	if err != nil || done {
		return err
	}
	_, err = j.RunOnce(ctx)
	return err
}

// notified 报告 date 的日报是否已推送（推送标记存在）。读标记失败返回错误，本周期不跑，下周期重试。
func (j *Job) notified(ctx context.Context, date string) (bool, error) {
	_, err := j.d.Objects.GetObject(ctx, j.d.Bucket, model.DailyNotifiedKey(date))
	switch {
	case errors.Is(err, model.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("quality: read notified marker %s: %w", date, err)
	}
	return true, nil
}

// Outcome 是一次 RunOnce 的结果（Ran=false 表示锁被他人持有）。Notified 为 true 表示本次推送了摘要（已推送过的日期补跑不再推）。
type Outcome struct {
	Ran       bool
	Date      string
	ReportKey string
	Notified  bool
	Farms     map[FarmKey]FarmStat
	Fact      FactStatus
}

// FarmKey 是农场标识（model.Scope 含切片，不能作 map 键）。
type FarmKey struct{ TenantID, FarmID uint64 }

func keyOf(s model.Scope) FarmKey { return FarmKey{TenantID: s.TenantID, FarmID: s.FarmID} }

type farmDay struct {
	scope model.Scope
	day   Day
}

// RunOnce 依次抽样、汇总、出日报、推送（手动补跑也走这里：日指标同键覆盖、日报重写，已推送过的日期不再推送）。
// 单农场时区非法记警告并跳过该农场，其余农场照常（坏数据不拖垮整轮）。
func (j *Job) RunOnce(ctx context.Context) (Outcome, error) {
	now := j.d.Clock.Now()
	out := Outcome{Date: PrevDay(now, j.d.ReportLoc).Date, Farms: map[FarmKey]FarmStat{}}
	farms, err := j.farmDays(ctx, now)
	if err != nil {
		return out, err
	}
	unjudged, ran, err := j.sample(ctx, farms)
	if err != nil || !ran {
		return out, err
	}
	ran, err = j.withLock(ctx, JobRollup, func(ctx context.Context, renew func(context.Context) error) error {
		return j.rollup(ctx, now, farms, unjudged, renew, &out)
	})
	out.Ran = ran
	return out, err
}

func (j *Job) farmDays(ctx context.Context, now time.Time) ([]farmDay, error) {
	list, err := j.d.Farms.FarmTimezones(ctx, qualitySystem)
	if err != nil {
		return nil, fmt.Errorf("quality: list farm timezones: %w", err)
	}
	out := make([]farmDay, 0, len(list))
	for _, f := range list {
		day, err := FarmDay(now, f.Timezone)
		if err != nil {
			j.d.Logger.WarnContext(ctx, "skip farm with bad timezone", "tenant_id", f.TenantID, "farm_id", f.FarmID,
				"timezone", f.Timezone, "err", err)
			continue
		}
		out = append(out, farmDay{scope: model.Scope{TenantID: f.TenantID, FarmID: f.FarmID}, day: day})
	}
	return out, nil
}

// sample 在 lock:p2job:online-judge-sample 下逐农场抽样；返回各农场当日是否「未评」。Sampler 未配置时全部未评。
func (j *Job) sample(ctx context.Context, farms []farmDay) (map[FarmKey]bool, bool, error) {
	unjudged := make(map[FarmKey]bool, len(farms))
	if j.d.Sampler == nil {
		for _, f := range farms {
			unjudged[keyOf(f.scope)] = true
		}
		return unjudged, true, nil
	}
	ran, err := j.withLock(ctx, JobSample, func(ctx context.Context, renew func(context.Context) error) error {
		for _, f := range farms {
			res, err := j.d.Sampler.Sample(ctx, f.scope, f.day, renew)
			if err != nil {
				return fmt.Errorf("quality: sample farm %d %s: %w", f.scope.FarmID, f.day.Date, err)
			}
			unjudged[keyOf(f.scope)] = res.Unjudged()
		}
		return nil
	})
	return unjudged, ran, err
}

func (j *Job) rollup(ctx context.Context, now time.Time, farms []farmDay, unjudged map[FarmKey]bool,
	renew func(context.Context) error, out *Outcome) error {
	dates := map[string]bool{out.Date: false} // 业务日 → 是否有农场未评
	for _, f := range farms {
		st, err := j.d.Rollup.Farm(ctx, qualitySystem, f.scope, f.day, unjudged[keyOf(f.scope)], now)
		if err != nil {
			return fmt.Errorf("quality: rollup farm %d %s: %w", f.scope.FarmID, f.day.Date, err)
		}
		out.Farms[keyOf(f.scope)] = st
		dates[f.day.Date] = dates[f.day.Date] || unjudged[keyOf(f.scope)]
		if err := renew(ctx); err != nil {
			return fmt.Errorf("quality: renew rollup lock: %w", err)
		}
	}
	off, err := LoadOffline(ctx, j.d.Objects, j.d.Bucket)
	if err != nil {
		return err
	}
	for date, anyUnjudged := range dates {
		var extra []model.QualityDaily
		if date == out.Date {
			extra = OfflineRows(date, off.Metrics)
		}
		fact, err := j.d.Rollup.Platform(ctx, qualitySystem, date, anyUnjudged, extra, now)
		if err != nil {
			return err
		}
		if date == out.Date {
			out.Fact = fact
		}
	}
	return j.report(ctx, farms, off, out)
}

func (j *Job) report(ctx context.Context, farms []farmDay, off Offline, out *Outcome) error {
	in := ReportInput{Date: out.Date, Offline: off, BaselineName: j.d.BaselineName, OnlineFact: out.Fact,
		OnlineRuns: map[string][]uint64{}}
	if j.d.BaselineID != 0 {
		b, err := LoadMetrics(ctx, j.d.Objects, j.d.Bucket, j.d.BaselineID)
		switch {
		case errors.Is(err, model.ErrNotFound):
			j.d.Logger.WarnContext(ctx, "baseline metrics.json missing, diffs shown as —", "experiment_id", j.d.BaselineID)
		case err != nil:
			return err
		default:
			in.Baseline = b
		}
	}
	rows, err := j.d.Reports.DailyRows(ctx, qualitySystem, 0, 0, out.Date)
	if err != nil {
		return err
	}
	in.Online = rows
	if err := j.drillRuns(ctx, farms, out.Date, in.OnlineRuns); err != nil {
		return err
	}
	rep := BuildReport(in)
	key, err := UploadReport(ctx, j.d.Objects, j.d.Bucket, rep)
	if err != nil {
		return err
	}
	out.ReportKey = key
	// 推送前在 rollup 锁内复查标记：多实例 / 补跑时已推送过的日期只重写日报、不再推送。
	done, err := j.notified(ctx, out.Date)
	if err != nil {
		return err
	}
	if done {
		j.d.Logger.InfoContext(ctx, "daily report already notified, skip notify", "date", out.Date)
		return nil
	}
	title, text := Summary(rep, key)
	if err := j.d.Notifier.Notify(ctx, title, text); err != nil {
		return fmt.Errorf("quality: notify daily report %s: %w", out.Date, err)
	}
	out.Notified = true
	// 推送成功后写标记；写标记失败则下周期重跑并可能重推一次（至少一次，不丢推送）。
	return j.markNotified(ctx, out.Date, key)
}

func (j *Job) markNotified(ctx context.Context, date, reportKey string) error {
	body, err := json.Marshal(model.DailyNotified{Date: date, ReportKey: reportKey, SentAt: j.d.Clock.Now().UTC()})
	if err != nil {
		return fmt.Errorf("quality: marshal notified marker %s: %w", date, err)
	}
	if err := j.d.Objects.PutObject(ctx, j.d.Bucket, model.DailyNotifiedKey(date), body, "application/json"); err != nil {
		return fmt.Errorf("quality: write notified marker %s: %w", date, err)
	}
	return nil
}

// drillFilters 是线上指标 → 下钻 run 的条件（未覆盖 / 拒收 / 拒答）。
var drillFilters = map[string]repository.RunFilter{
	model.MetricCitationCoverage: repository.RunsUncited,
	model.MetricNumericAccuracy:  repository.RunsValidationFailed,
	model.MetricRefusalRate:      repository.RunsRefused,
}

// drillRuns 收集日报日各农场的下钻 run_id（每个指标最多 drillRunsPerRow 个）；事实一致率列待专家（NEED_EXPERT）的 run。
func (j *Job) drillRuns(ctx context.Context, farms []farmDay, date string, runs map[string][]uint64) error {
	for _, f := range farms {
		if f.day.Date != date {
			continue
		}
		for metric, filter := range drillFilters {
			left := drillRunsPerRow - len(runs[metric])
			if left <= 0 {
				continue
			}
			ids, err := j.d.Reports.AdviceRuns(ctx, f.scope, f.day.From, f.day.To, filter, left)
			if err != nil {
				return err
			}
			runs[metric] = append(runs[metric], ids...)
		}
		res, err := j.d.Reports.JudgeResults(ctx, f.scope, date, judge.DimFactConsistency)
		if err != nil {
			return err
		}
		for _, r := range res {
			if r.Status == model.JudgeStatusNeedExpert && len(runs[model.MetricFactConsistency]) < drillRunsPerRow {
				runs[model.MetricFactConsistency] = append(runs[model.MetricFactConsistency], r.RunID)
			}
		}
	}
	return nil
}

// withLock 持 lock:p2job:{job} 执行 fn；锁被他人持有返回 ran=false。fn 收到的 renew 续期锁。
func (j *Job) withLock(ctx context.Context, job string, fn func(ctx context.Context, renew func(context.Context) error) error) (bool, error) {
	lease, err := j.d.Locks.AcquireLease(ctx, j.d.Keys.LockP2Job(job), rediskey.LockP2JobTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		j.d.Logger.InfoContext(ctx, "p2 job lock held, skip", "job", job)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("quality: lock %s: %w", job, err)
	}
	defer func() {
		if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
			j.d.Logger.WarnContext(ctx, "release p2 job lock", "job", job, "err", rerr)
		}
	}()
	return true, fn(ctx, lease.Renew)
}
