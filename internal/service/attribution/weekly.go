package attribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// JobWeekly 是周报任务名（lock:p2job:attribution-weekly 的 job 名）。
const JobWeekly = "attribution-weekly"

// JobDeps 是周报任务依赖；Experts 可空（待调度方），其余必填。System 由 app 构造（archtest 白名单）。
type JobDeps struct {
	Farms         FarmLister
	Hits          HitStore
	Issues        IssueStore
	Experts       ExpertReviewSource
	Objects       Objects
	Bucket        string
	Locks         Locker
	Keys          rediskey.Keys
	Clock         clock.Clock
	Logger        *slog.Logger
	System        repository.SystemScope
	Loc           *time.Location // 周界时区
	TopN          int
	Interval      time.Duration
	KBLoadGapsKey string // 空表示不读，周报该节写「无」
}

// ErrJobDeps 表示周报任务依赖缺失。
var ErrJobDeps = errors.New("attribution: weekly job deps invalid")

// Job 是归因周报任务。「上周已出」不放进程内存，而看 eval 桶 done 标记：重启、多实例都只出一次。
type Job struct{ d JobDeps }

// NewJob 校验依赖。
func NewJob(d JobDeps) (*Job, error) {
	if d.Farms == nil || d.Hits == nil || d.Issues == nil || d.Objects == nil || d.Bucket == "" || d.Locks == nil ||
		d.Clock == nil || d.Loc == nil || d.TopN <= 0 {
		return nil, fmt.Errorf("%w: needs Farms, Hits, Issues, Objects, Bucket, Locks, Clock, Loc and TopN > 0", ErrJobDeps)
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Interval <= 0 {
		d.Interval = time.Hour
	}
	return &Job{d: d}, nil
}

// Week 是一个 ISO 周的 [From, To)（周界时区下周一 00:00 起）。
type Week struct {
	Name     string // 2026W45
	From, To time.Time
}

// PrevWeek 是 now 在 loc 时区下的上一 ISO 周。
func PrevWeek(now time.Time, loc *time.Location) Week {
	this := model.WeekStart(now.In(loc))
	from := this.AddDate(0, 0, -7)
	return Week{Name: model.ISOWeek(from), From: from, To: this}
}

// ReportKey 是周报对象键 report/weekly/{week}.html。
func ReportKey(week string) string { return "report/weekly/" + week + ".html" }

// DoneKey 是周报 done 标记 report/weekly/{week}.json（内容为 Top 问题快照）。
func DoneKey(week string) string { return "report/weekly/" + week + ".json" }

// Run 启动即检查一次，之后每个周期检查，直到 ctx 取消。
func (j *Job) Run(ctx context.Context) {
	t := time.NewTicker(j.d.Interval)
	defer t.Stop()
	for {
		if err := j.Tick(ctx); err != nil && ctx.Err() == nil {
			j.d.Logger.WarnContext(ctx, "attribution weekly job", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick 在上一 ISO 周尚无 done 标记时执行 RunOnce（周一首个周期即出上周周报；本周内某次失败或 done 标记写失败，
// 下个周期重出）。只看「上一 ISO 周」：整周停机漏掉的更早周不会自动补出。
func (j *Job) Tick(ctx context.Context) error {
	w := PrevWeek(j.d.Clock.Now(), j.d.Loc)
	if _, ok, err := j.snapshot(ctx, w.Name); err != nil || ok {
		return err
	}
	_, err := j.RunOnce(ctx)
	return err
}

// Snapshot 是 done 标记内容：本期 Top 问题当时的状态（下周据此回填处置）。
type Snapshot struct {
	Week        string          `json:"week"`
	ReportKey   string          `json:"report_key"`
	GeneratedAt time.Time       `json:"generated_at"`
	Top         []SnapshotIssue `json:"top"`
	Reopened    []string        `json:"reopened"` // 本周由 RESOLVED / VERIFIED 重开的签名（补跑时沿用）
}

// SnapshotIssue 是快照里的一个问题。
type SnapshotIssue struct {
	ID          uint64  `json:"id"`
	Signature   string  `json:"signature"`
	HitCount    int     `json:"hit_count"`
	Status      string  `json:"status"`
	Disposition *string `json:"disposition"`
	OwnerID     *uint64 `json:"owner_id"`
}

// snapshot 读 week 的 done 标记；不存在返回 ok=false。读失败返回错误，本周期不跑，下周期重试。
func (j *Job) snapshot(ctx context.Context, week string) (Snapshot, bool, error) {
	raw, err := j.d.Objects.GetObject(ctx, j.d.Bucket, DoneKey(week))
	switch {
	case errors.Is(err, model.ErrNotFound):
		return Snapshot{}, false, nil
	case err != nil:
		return Snapshot{}, false, fmt.Errorf("attribution: read done marker %s: %w", week, err)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return Snapshot{}, false, fmt.Errorf("attribution: decode done marker %s: %w", week, err)
	}
	return s, true, nil
}

// Outcome 是一次 RunOnce 的结果（Ran=false：锁被他人持有，或 Done=true 取锁后发现本周已出）。
type Outcome struct {
	Ran       bool
	Done      bool // 取锁后复查到 done 标记（另一副本已出完），本次未跑
	Week      string
	ReportKey string
	Hits      int      // 本周信号条数
	Issues    int      // 本周命中的签名数
	Reopened  []string // 本周由 RESOLVED / VERIFIED 重开的签名
}

// RunOnce 持 lock:p2job:attribution-weekly 取上周信号、upsert 平台行、出周报、写 done 标记。取锁后复查 done 标记，
// 已有即返回（Tick 查标记与取锁之间另一副本可能已出完）。done 标记写失败后的重出也走这里：同签名同周重跑覆盖命中数、
// 证据按来源行去重，不重复计重开。
func (j *Job) RunOnce(ctx context.Context) (Outcome, error) {
	w := PrevWeek(j.d.Clock.Now(), j.d.Loc)
	out := Outcome{Week: w.Name}
	lease, err := j.d.Locks.AcquireLease(ctx, j.d.Keys.LockP2Job(JobWeekly), rediskey.LockP2JobTTL)
	if errors.Is(err, repository.ErrLockHeld) {
		j.d.Logger.InfoContext(ctx, "p2 job lock held, skip", "job", JobWeekly)
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("attribution: lock %s: %w", JobWeekly, err)
	}
	defer func() {
		if rerr := lease.Release(context.WithoutCancel(ctx)); rerr != nil {
			j.d.Logger.WarnContext(ctx, "release p2 job lock", "job", JobWeekly, "err", rerr)
		}
	}()
	if _, done, err := j.snapshot(ctx, w.Name); err != nil {
		return out, err
	} else if done {
		j.d.Logger.InfoContext(ctx, "p2 job already done, skip", "job", JobWeekly, "week", w.Name)
		out.Done = true
		return out, nil
	}
	out.Ran = true
	return out, j.run(ctx, w, lease.Renew, &out)
}

// issueBatch 是一次 IN 查询 / upsert 事务的签名数上限。
const issueBatch = 500

// run 逐农场取信号（每农场后续租）、聚合、按批 upsert，再出周报与 done 标记。
func (j *Job) run(ctx context.Context, w Week, renew func(context.Context) error, out *Outcome) error {
	farms, err := j.d.Farms.FarmTimezones(ctx, j.d.System)
	if err != nil {
		return fmt.Errorf("attribution: list farms: %w", err)
	}
	var hits []model.AttributionHit
	flow := map[string]int{}
	for _, f := range farms {
		scope := model.Scope{TenantID: f.TenantID, FarmID: f.FarmID}
		h, err := j.d.Hits.WeekHits(ctx, scope, w.From, w.To)
		if err != nil {
			return fmt.Errorf("attribution: hits farm %d %s: %w", f.FarmID, w.Name, err)
		}
		hits = append(hits, h...)
		counts, err := j.d.Hits.BackflowCounts(ctx, scope, w.From, w.To)
		if err != nil {
			return fmt.Errorf("attribution: backflow counts farm %d %s: %w", f.FarmID, w.Name, err)
		}
		for status, n := range counts {
			flow[status] += n
		}
		if err := renew(ctx); err != nil {
			return fmt.Errorf("attribution: renew lock: %w", err)
		}
	}
	if j.d.Experts != nil {
		h, err := j.d.Experts.WeekHits(ctx, w.From, w.To)
		if err != nil {
			return fmt.Errorf("attribution: expert review hits %s: %w", w.Name, err)
		}
		hits = append(hits, h...)
	}
	ups := model.AggregateHits(w.Name, hits)
	out.Hits, out.Issues = len(hits), len(ups)
	for start := 0; start < len(ups); start += issueBatch {
		reopened, err := j.upsert(ctx, ups[start:min(start+issueBatch, len(ups))])
		if err != nil {
			return err
		}
		out.Reopened = append(out.Reopened, reopened...)
		if err := renew(ctx); err != nil {
			return fmt.Errorf("attribution: renew lock: %w", err)
		}
	}
	return j.report(ctx, w, flow, out)
}

// upsert 合并旧证据（本周在前、去重、截 50）后写一批平台行，返回其中由已关闭重开的签名。
func (j *Job) upsert(ctx context.Context, ups []model.IssueUpsert) ([]string, error) {
	sigs := make([]string, len(ups))
	for i, u := range ups {
		sigs[i] = u.Signature
	}
	old, err := j.d.Issues.IssuesBySignature(ctx, j.d.System, sigs)
	if err != nil {
		return nil, fmt.Errorf("attribution: read issues: %w", err)
	}
	bySig := make(map[string]model.AttributionIssue, len(old))
	for _, o := range old {
		bySig[o.Signature] = o
	}
	var reopened []string
	for i := range ups {
		o, ok := bySig[ups[i].Signature]
		ups[i].Evidence = model.MergeEvidence(ups[i].Evidence, o.Evidence)
		if ok && o.Closed() {
			reopened = append(reopened, o.Signature)
		}
	}
	if err := j.d.Issues.UpsertIssues(ctx, j.d.System, ups, j.d.Clock.Now()); err != nil {
		return nil, fmt.Errorf("attribution: upsert issues: %w", err)
	}
	return reopened, nil
}
