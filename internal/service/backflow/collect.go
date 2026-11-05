package backflow

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// JobCollect 是回流任务名（lock:p2job:backflow-collect 的 job 名）。
const JobCollect = "backflow-collect"

// JobDeps 是回流任务依赖；Logger / Interval / Rand 可空，其余必填。System 由 app 构造（archtest 白名单）。
type JobDeps struct {
	Farms  FarmLister
	Hits   HitStore
	Store  Store
	Raw    RawReader
	Fetch  Fetcher
	Locks  Locker
	Keys   rediskey.Keys
	Clock  clock.Clock
	Logger *slog.Logger
	System repository.SystemScope
	Loc    *time.Location // 周界时区；农场时区为空或不可加载时草稿 clock 也用它
	// Interval 是检查周期（缺省 1h）：每个上一 ISO 周在本进程只跑一次，重启后重跑由 backflow_status 保证幂等。
	Interval      time.Duration
	DraftsDir     string
	KBMissMinHits int    // 「高频 KB_MISS」阈值（同农场同问题类型一周的拒答次数，待线上数据）
	MaxRuns       int    // 单农场每周最多选样 / 每轮最多出草稿的 run 数
	ServiceUserID uint64 // 读原文的服务账号（RAW_READ 审计 actor）
	Rand          io.Reader
}

// ErrJobDeps 表示回流任务依赖缺失。
var ErrJobDeps = errors.New("backflow: collect job deps invalid")

// Job 是 backflow-collect 任务。
type Job struct {
	d        JobDeps
	lastWeek string // 本进程已跑完的上一 ISO 周（只由 Run 的单个 goroutine 读写）
}

// NewJob 校验依赖。
func NewJob(d JobDeps) (*Job, error) {
	if d.Farms == nil || d.Hits == nil || d.Store == nil || d.Raw == nil || d.Fetch == nil || d.Locks == nil ||
		d.Clock == nil || d.Loc == nil || d.DraftsDir == "" || d.KBMissMinHits <= 0 || d.MaxRuns <= 0 ||
		d.ServiceUserID == 0 {
		return nil, fmt.Errorf("%w: needs Farms, Hits, Store, Raw, Fetch, Locks, Clock, Loc, DraftsDir, "+
			"KBMissMinHits > 0, MaxRuns > 0 and ServiceUserID", ErrJobDeps)
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Interval <= 0 {
		d.Interval = time.Hour
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	return &Job{d: d}, nil
}

// Run 启动即检查一次，之后每个周期检查，直到 ctx 取消。
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.d.Interval)
	defer t.Stop()
	for {
		if err := j.Tick(ctx); err != nil && ctx.Err() == nil {
			j.d.Logger.WarnContext(ctx, "backflow collect job", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 在上一 ISO 周本进程尚未跑完时执行 RunOnce（取数 / 写库出错或锁被他人持有时下周期重试）。
// 单个 run 出草稿失败不算任务出错：该 run 记一次尝试、保持 SELECTED，下次 RunOnce（下周或重启后）再试。
func (j *Job) Tick(ctx context.Context) error {
	week := model.ISOWeek(j.weekFrom())
	if week == j.lastWeek {
		return nil
	}
	out, err := j.RunOnce(ctx)
	if err == nil && out.Ran {
		j.lastWeek = week
	}
	return err
}

// weekFrom 是上一 ISO 周的周一 00:00（周界时区）。
func (j *Job) weekFrom() time.Time {
	return model.WeekStart(j.d.Clock.Now().In(j.d.Loc)).AddDate(0, 0, -7)
}

// Outcome 是一次 RunOnce 的结果（Ran=false 表示锁被他人持有）。
type Outcome struct {
	Ran       bool
	Week      string
	Selected  int64 // 本轮新置 SELECTED 的 run 数
	Drafted   int   // 本轮写出草稿（DRAFTED）
	PIIFailed int   // 本轮 pii_clean 未过（PII_FAILED）
	Retrying  int   // 本轮出草稿失败、保持 SELECTED 待下次再试
	Failed    int   // 本轮失败次数达 model.BackflowMaxAttempts、置 FAILED
}

// RunOnce 持 lock:p2job:backflow-collect 逐农场选样、出草稿（每农场后续租）。单个 run 出草稿失败只记一次尝试
// （达上限置 FAILED，不再占名额），不影响其余 run、也不作为任务错误返回；取数 / 写库错误合并返回。
func (j *Job) RunOnce(ctx context.Context) (Outcome, error) {
	from := j.weekFrom()
	out := Outcome{Week: model.ISOWeek(from)}
	lease, err := j.d.Locks.AcquireLease(ctx, j.d.Keys.LockP2Job(JobCollect), rediskey.LockP2JobTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		j.d.Logger.InfoContext(ctx, "p2 job lock held, skip", "job", JobCollect)
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("backflow: lock %s: %w", JobCollect, err)
	}
	defer func() {
		if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
			j.d.Logger.WarnContext(ctx, "release p2 job lock", "job", JobCollect, "err", rerr)
		}
	}()
	out.Ran = true
	farms, err := j.d.Farms.FarmTimezones(ctx, j.d.System)
	if err != nil {
		return out, fmt.Errorf("backflow: list farms: %w", err)
	}
	var errs []error
	for _, f := range farms {
		if err := j.farm(ctx, f, from, &out); err != nil {
			errs = append(errs, err)
		}
		if err := lease.Renew(ctx); err != nil {
			return out, errors.Join(append(errs, fmt.Errorf("backflow: renew lock: %w", err))...)
		}
	}
	return out, errors.Join(errs...)
}

// farm 选出本农场上周的回流 run 并置 SELECTED，再为本农场 SELECTED 的 run（含上轮中断留下的）出草稿。
func (j *Job) farm(ctx context.Context, f repository.FarmTZ, from time.Time, out *Outcome) error {
	scope := model.Scope{TenantID: f.TenantID, FarmID: f.FarmID}
	hits, err := j.d.Hits.WeekHits(ctx, scope, from, from.AddDate(0, 0, 7))
	if err != nil {
		return fmt.Errorf("backflow: hits farm %d: %w", f.FarmID, err)
	}
	now := j.d.Clock.Now()
	if ids := pick(scope, hits, j.d.KBMissMinHits, j.d.MaxRuns); len(ids) > 0 {
		n, err := j.d.Store.MarkSelected(ctx, scope, ids, now)
		if err != nil {
			return fmt.Errorf("backflow: select farm %d: %w", f.FarmID, err)
		}
		out.Selected += n
	}
	runs, err := j.d.Store.SelectedRuns(ctx, scope, j.d.MaxRuns)
	if err != nil {
		return fmt.Errorf("backflow: selected runs farm %d: %w", f.FarmID, err)
	}
	loc := j.d.Loc
	if l, err := time.LoadLocation(f.Timezone); f.Timezone != "" && err == nil {
		loc = l
	}
	var errs []error
	for _, runID := range runs {
		status, err := j.draft(ctx, scope, runID, loc)
		switch {
		case err != nil:
			if ferr := j.failAttempt(ctx, scope, runID, err, out); ferr != nil {
				errs = append(errs, ferr)
			}
		case status == model.BackflowDrafted:
			out.Drafted++
		default:
			out.PIIFailed++
		}
	}
	return errors.Join(errs...)
}

// failAttempt 记一次失败尝试（达上限置 FAILED）。只有记录本身失败才返回错误。
func (j *Job) failAttempt(ctx context.Context, scope model.Scope, runID uint64, cause error, out *Outcome) error {
	failed, err := j.d.Store.FailAttempt(ctx, scope, runID, model.BackflowMaxAttempts, j.d.Clock.Now())
	if err != nil {
		return fmt.Errorf("backflow: farm %d run %d: record attempt (cause %v): %w", scope.FarmID, runID, cause, err)
	}
	j.d.Logger.WarnContext(ctx, "backflow draft failed", "farm_id", scope.FarmID, "run_id", runID, "failed", failed,
		"err", cause)
	if failed {
		out.Failed++
	} else {
		out.Retrying++
	}
	return nil
}

// pick 按回流口径从本农场信号里选 run（保持信号次序、去重、最多 max 个）：差评、专家判错、护栏拦截直接入选；
// KB_MISS 拒答只在同问题类型一周达到 kbMissMin 次时入选；驳回不进回流（已有结构化原因进归因）。
// 不属于 scope 的信号一律丢弃（仓储按 Scope 取数之外再挡一层）。
func pick(scope model.Scope, hits []model.AttributionHit, kbMissMin, limit int) []uint64 {
	kbMiss := map[string]int{}
	for _, h := range hits {
		if h.Source == model.EvidenceKBMiss && h.TenantID == scope.TenantID && h.FarmID == scope.FarmID {
			kbMiss[h.QuestionType]++
		}
	}
	seen := map[uint64]bool{}
	var ids []uint64
	for _, h := range hits {
		if len(ids) >= limit {
			break
		}
		if h.TenantID != scope.TenantID || h.FarmID != scope.FarmID || h.RunID == 0 || seen[h.RunID] {
			continue
		}
		switch h.Source {
		case model.EvidenceFeedback, model.EvidenceExpert, model.EvidenceValidation:
		case model.EvidenceKBMiss:
			if kbMiss[h.QuestionType] < kbMissMin {
				continue
			}
		default:
			continue
		}
		seen[h.RunID] = true
		ids = append(ids, h.RunID)
	}
	return ids
}

// serviceScope 是读原文的服务账号凭证（auditor 持 raw.read；RAW_READ 审计记该账号）。
func (j *Job) serviceScope(scope model.Scope) model.Scope {
	return model.Scope{TenantID: scope.TenantID, FarmID: scope.FarmID, UserID: j.d.ServiceUserID,
		Roles: []model.Role{model.RoleAuditor}}
}

// draft 为一个 SELECTED 的 run 出草稿：逐条 ReadRaw（每条一次 RAW_READ）→ 取原文 → 脱敏 → 兜底扫描；
// 通过写 {DraftsDir}/{draft_ref}.yaml 后置 DRAFTED（映射只写 agent_run.backflow_draft_ref），不过置 PII_FAILED 不写文件。
// 读原文出错（过保留期除外）返回错误，由 failAttempt 记尝试次数。
func (j *Job) draft(ctx context.Context, scope model.Scope, runID uint64, loc *time.Location) (string, error) {
	m, err := j.d.Store.Material(ctx, scope, runID)
	if err != nil {
		return "", err
	}
	sc, err := newScrubber(j.d.Rand, scope.FarmID, m.FieldID, m.UserID)
	if err != nil {
		return "", err
	}
	in := draftInput{crop: m.Crop, kbRef: m.KnowledgeRef, clock: m.AskedAt.In(loc)}
	if in.ref, err = sc.draftRef(); err != nil {
		return "", err
	}
	if in.question, err = sc.text(m.Question); err != nil {
		return "", err
	}
	svc := j.serviceScope(scope)
	for _, t := range m.Tools {
		ret, ok, err := j.toolReturn(ctx, svc, sc, runID, t)
		if err != nil {
			return "", err
		}
		if !ok {
			in.skipped = append(in.skipped, t.Name)
			continue
		}
		in.tools = append(in.tools, toolReturn{name: t.Name, ret: ret})
	}
	doc := buildDraft(sc, in)
	body, err := encodeDraft(doc)
	if err != nil {
		return "", err
	}
	now := j.d.Clock.Now()
	if leak, bad := gate(doc, body, materialRealIDs(scope.TenantID, scope.FarmID, m)); bad {
		j.d.Logger.WarnContext(ctx, "backflow draft pii_clean failed", "run_id", runID, "where", leak.where, "kind", leak.kind)
		if err := j.d.Store.FinishDraft(ctx, scope, runID, model.BackflowPIIFailed, "", now); err != nil {
			return "", err
		}
		return model.BackflowPIIFailed, nil
	}
	path, err := writeDraft(j.d.DraftsDir, in.ref, body)
	if err != nil {
		return "", err
	}
	if err := j.d.Store.FinishDraft(ctx, scope, runID, model.BackflowDrafted, in.ref, now); err != nil {
		if rerr := os.Remove(path); rerr != nil {
			err = errors.Join(err, fmt.Errorf("backflow: remove orphan draft: %w", rerr))
		}
		return "", err
	}
	return model.BackflowDrafted, nil
}

// toolReturn 读一条工具原文并脱敏；原文过保留期或无法解析返回 ok=false（草稿 notes 注明跳过）。
func (j *Job) toolReturn(ctx context.Context, svc model.Scope, sc *scrubber, runID uint64, t model.BackflowTool) (any, bool, error) {
	link, err := j.d.Raw.ReadRaw(ctx, svc, runID, t.RawID)
	if errors.Is(err, model.ErrRawExpired) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read raw %d: %w", t.RawID, err)
	}
	body, err := j.d.Fetch.Fetch(ctx, link.URL)
	if err != nil {
		return nil, false, fmt.Errorf("fetch raw %d: %w", t.RawID, err)
	}
	ret, err := sc.fixtureReturn(body)
	if errors.Is(err, ErrRandom) {
		return nil, false, err
	}
	if err != nil {
		j.d.Logger.WarnContext(ctx, "backflow tool raw undecodable, skipped", "run_id", runID, "raw_id", t.RawID, "err", err)
		return nil, false, nil
	}
	return ret, true, nil
}

// writeDraft 原子写 {dir}/{ref}.yaml（先写同目录临时文件再改名，崩溃不留半截草稿）。
func writeDraft(dir, ref string, body []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("backflow: drafts dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".draft-*")
	if err != nil {
		return "", fmt.Errorf("backflow: create draft: %w", err)
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return "", errors.Join(fmt.Errorf("backflow: write draft: %w", err), os.Remove(tmp.Name()))
	}
	path := filepath.Join(dir, ref+".yaml")
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", errors.Join(fmt.Errorf("backflow: publish draft: %w", err), os.Remove(tmp.Name()))
	}
	return path, nil
}
