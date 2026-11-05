//go:build integration

package backflow_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
	"agri-steward/internal/service/attribution"
	"agri-steward/internal/service/backflow"
)

// 构造数据（无线上数据，待线上数据）：示范农场 2026W45–W48 四周，周界 Asia/Shanghai。
var (
	cst = time.FixedZone("CST", 8*3600)
	w45 = time.Date(2026, 11, 2, 0, 0, 0, 0, cst) // W45 周一
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// itPresign 签发可预测的 URL（对象键即 URL 路径）。
type itPresign struct{}

func (itPresign) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://minio.it/" + key + "?sig=1", nil
}

// itFetch 按 itPresign 的 URL 取原文。
type itFetch map[string][]byte

func (f itFetch) Fetch(_ context.Context, url string) ([]byte, error) {
	key := strings.TrimSuffix(strings.TrimPrefix(url, "https://minio.it/"), "?sig=1")
	b, ok := f[key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

// countingRaw 包住真实 AuditService.ReadRaw，记调用次数（与 audit_log 的 RAW_READ 行数对账）。
type countingRaw struct {
	svc *service.AuditService
	n   int
}

func (c *countingRaw) ReadRaw(ctx context.Context, scope model.Scope, runID, rawID uint64) (service.RawLink, error) {
	c.n++
	return c.svc.ReadRaw(ctx, scope, runID, rawID)
}

type itObjects map[string][]byte

func (o itObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	b, ok := o[bucket+"/"+key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func (o itObjects) PutObject(_ context.Context, bucket, key string, body []byte, _ string) error {
	o[bucket+"/"+key] = body
	return nil
}

func exec(t *testing.T, db *repository.DB, q string, args ...any) {
	t.Helper()
	if err := db.WithContext(t.Context()).Exec(q, args...).Error; err != nil {
		t.Fatal(err)
	}
}

// weekly 是四周集成的夹具：真实 AttributionRepo / BackflowRepo / AuditService / Redis 锁，注入时钟。
type weekly struct {
	t        *testing.T
	db       *repository.DB
	clk      interface{ Set(time.Time) }
	repo     repository.AttributionRepo
	sys      repository.SystemScope
	attr     *attribution.Job
	flow     *backflow.Job
	raw      *countingRaw
	fetch    itFetch
	objects  itObjects
	draftDir string
}

// run 建一个有 ASK 审计、一条 OK 工具调用的 run；rawRun 非 0 时工具原文挂在该 run 名下（ReadRaw 判跨 run → 拒绝）。
func (w *weekly) run(rawRun uint64) uint64 {
	t := w.t
	id := itest.InsertRun(t, w.db, w45)
	owner := id
	if rawRun != 0 {
		owner = rawRun
	}
	key := fmt.Sprintf("raw/it-l8/tool-%d", id)
	exec(t, w.db, `INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key, sha256, size_bytes, created_at)
  VALUES (?, ?, ?, ?, ?, REPEAT('a', 64), 10, ?)`, itest.TenantID, itest.FarmID, owner, model.RawToolRaw, key, w45)
	rawID := uint64(itest.Count(t, w.db, `SELECT MAX(id) FROM raw_object`))
	w.fetch[key] = []byte(`{"data":{"wind_speed_ms":2,"source":"mock-station"}}`)
	exec(t, w.db, `INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, tool_name, policy_level,
  args_digest, status, raw_object_id, latency_ms, created_at)
  VALUES (?, ?, ?, 'enrich', LPAD(?, 32, '0'), 1, 'weather.forecast', 'read', REPEAT('0', 64), ?, ?, 10, ?)`,
		itest.TenantID, itest.FarmID, id, id, model.ToolStatusOK, rawID, w45)
	exec(t, w.db, `INSERT INTO audit_log (created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, target,
  payload_digest, summary, result) VALUES (?, ?, ?, ?, 'user', 1001, 'ASK', 'field:1', REPEAT('0', 64), ?, 'OK')`,
		w45, itest.TenantID, itest.FarmID, id, `{"question":"稻飞虱要打药吗","field_id":1}`)
	return id
}

// dislike 在 at 给 run 出一张卡并记一条差评（category 即归因原因，卡类别 pest、无 kb 引用）。
func (w *weekly) dislike(run uint64, category model.RejectReasonCode, at time.Time) {
	t := w.t
	exec(t, w.db, `INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, next_action, schema_ver, created_at)
  VALUES (?, ?, 1, ?, 'rice', 'tillering', 'pest', 'spray', 'OK', '注意稻飞虱', '[]', 'NONE', 1, ?)`,
		itest.TenantID, itest.FarmID, run, at.UTC())
	card := uint64(itest.Count(t, w.db, `SELECT MAX(id) FROM advice_card WHERE run_id = ?`, run))
	exec(t, w.db, `INSERT INTO advice_feedback (tenant_id, farm_id, advice_id, run_id, user_id, verdict, category, request_id,
  created_at) VALUES (?, ?, ?, ?, 1001, 'DISLIKE', ?, ?, ?)`, itest.TenantID, itest.FarmID, card, run, category,
		fmt.Sprintf("it-l8w-%d", run), at.UTC())
}

// monday 把时钟拨到 week 之后的周一 01:00，依次 Tick 周报与回流（与 worker 同一入口）。
func (w *weekly) monday(week time.Time) {
	w.t.Helper()
	w.clk.Set(week.AddDate(0, 0, 7).Add(time.Hour).UTC())
	if err := w.attr.Tick(w.t.Context()); err != nil {
		w.t.Fatalf("attribution tick %s: %v", model.ISOWeek(week), err)
	}
	if err := w.flow.Tick(w.t.Context()); err != nil {
		w.t.Fatalf("backflow tick %s: %v", model.ISOWeek(week), err)
	}
}

// issue 读签名的平台行，并断言跨周只有一行。
func (w *weekly) issue(sig string) model.AttributionIssue {
	w.t.Helper()
	if n := itest.Count(w.t, w.db, `SELECT COUNT(*) FROM attribution_issue WHERE tenant_id = 0 AND farm_id = 0 AND signature = ?`,
		sig); n != 1 {
		w.t.Fatalf("attribution_issue rows for %s = %d, want 1", sig, n)
	}
	got, err := w.repo.IssuesBySignature(w.t.Context(), w.sys, []string{sig})
	if err != nil || len(got) != 1 {
		w.t.Fatalf("IssuesBySignature = %+v, %v", got, err)
	}
	return got[0]
}

// P2 L8 四周集成（审查 P1-1）：worker 同一入口 Tick，真实仓储与审计。断言：跨周同一签名只有一行；hit_count 是该周命中数；
// RESOLVED 后复发 reopen_count+1、未关闭再命中不加；周报回流量按 BACKFLOW 事件时刻计；原文读不出的 run 每周记一次尝试，
// 第 3 次置 FAILED 后不再占 MaxRuns 名额；RAW_READ 审计行数与 ReadRaw 调用次数一致。
func TestL8FourWeeks_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl8weeks")
	slot := e.Fresh(t, w45)
	perms, err := service.LoadPermissionMatrix(filepath.Join(e.Root, "policy/permission_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := service.NewAuditService(service.AuditDeps{Store: repository.RawAccessRepo{DB: slot.DB}, Presign: itPresign{},
		Clock: slot.Clock, Logger: quiet(), Perms: perms})
	if err != nil {
		t.Fatal(err)
	}
	w := &weekly{t: t, db: slot.DB, clk: slot.Clock, repo: repository.AttributionRepo{DB: slot.DB},
		sys: repository.NewSystemScope(attribution.JobWeekly), raw: &countingRaw{svc: audit}, fetch: itFetch{},
		objects: itObjects{}, draftDir: t.TempDir()}
	locks := repository.NewLocker(e.Redis)
	if w.attr, err = attribution.NewJob(attribution.JobDeps{Farms: repository.QualityRepo{DB: slot.DB}, Hits: w.repo,
		Issues: w.repo, Objects: w.objects, Bucket: "eval", Locks: locks, Keys: slot.Keys, Clock: slot.Clock,
		Logger: quiet(), System: w.sys, Loc: cst, TopN: 20}); err != nil {
		t.Fatal(err)
	}
	const maxRuns = 2
	svcUser := uint64(config.DefaultBackflowServiceUser)
	if w.flow, err = backflow.NewJob(backflow.JobDeps{Farms: repository.QualityRepo{DB: slot.DB}, Hits: w.repo,
		Store: repository.BackflowRepo{DB: slot.DB}, Raw: w.raw, Fetch: w.fetch, Locks: locks, Keys: slot.Keys,
		Clock: slot.Clock, Logger: quiet(), System: repository.NewSystemScope(backflow.JobCollect), Loc: cst,
		DraftsDir: w.draftDir, KBMissMinHits: 3, MaxRuns: maxRuns, ServiceUserID: svcUser}); err != nil {
		t.Fatal(err)
	}

	other := w.run(0)
	r1, bad := w.run(0), w.run(other) // bad 的工具原文属 other：ReadRaw 判跨 run 拒绝（DENIED 审计）
	r3, r4, r5, r6 := w.run(0), w.run(0), w.run(0), w.run(0)
	dosage := model.Signature(string(model.RejectDosage), "pest", "")
	timing := model.Signature(string(model.RejectTiming), "pest", "")
	tue := func(week time.Time) time.Time { return week.AddDate(0, 0, 1).Add(10 * time.Hour) }
	status := func(run uint64) string {
		var s *string
		if err := slot.DB.WithContext(t.Context()).Raw(`SELECT backflow_status FROM agent_run WHERE id = ?`, run).
			Scan(&s).Error; err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return ""
		}
		return *s
	}

	// W45：DOSAGE 两次（r1、bad）→ 新行 hit_count=2；回流选 r1、bad，r1 出草稿，bad 第 1 次失败仍 SELECTED。
	w.dislike(r1, model.RejectDosage, tue(w45))
	w.dislike(bad, model.RejectDosage, tue(w45).Add(time.Hour))
	w.monday(w45)
	i := w.issue(dosage)
	if i.FirstWeek != "2026W45" || i.LastWeek != "2026W45" || i.HitCount != 2 || i.ReopenCount != 0 || i.Status != model.IssueOpen {
		t.Fatalf("W45 issue = %+v", i)
	}
	if status(r1) != model.BackflowDrafted || status(bad) != model.BackflowSelected {
		t.Fatalf("W45 backflow r1=%q bad=%q", status(r1), status(bad))
	}
	s := func(v string) *string { return &v }
	if _, err := w.repo.PatchIssue(t.Context(), w.sys, i.ID, model.AttributionPatch{Disposition: s(model.DispositionAddKB),
		ResolutionRef: s("kb PR #1"), Status: s(model.IssueResolved), RowVer: &i.RowVer}, w45.AddDate(0, 0, 8)); err != nil {
		t.Fatal(err)
	}

	// W46：RESOLVED 后复发一次 → 同一行 OPEN、hit_count=1、reopen_count=1；bad 第 2 次失败。
	w.dislike(r3, model.RejectDosage, tue(w45.AddDate(0, 0, 7)))
	w.monday(w45.AddDate(0, 0, 7))
	if i = w.issue(dosage); i.LastWeek != "2026W46" || i.HitCount != 1 || i.ReopenCount != 1 || i.Status != model.IssueOpen {
		t.Fatalf("W46 issue = %+v", i)
	}
	if status(r3) != model.BackflowDrafted || status(bad) != model.BackflowSelected {
		t.Fatalf("W46 backflow r3=%q bad=%q", status(r3), status(bad))
	}
	// W46 周报的回流量取 W46 窗口内的 BACKFLOW 事件：W45 那轮（W46 周一执行）选入 r1、bad。
	rep := string(w.objects["eval/"+attribution.ReportKey("2026W46")])
	if !strings.Contains(rep, "本周选入回流 2 个 run") {
		t.Fatalf("W46 report backflow section wrong:\n%s", rep)
	}

	// W47：只有 TIMING → 新签名一行；DOSAGE 行不动；bad 第 3 次失败 → FAILED。
	w.dislike(r4, model.RejectTiming, tue(w45.AddDate(0, 0, 14)))
	w.monday(w45.AddDate(0, 0, 14))
	if i = w.issue(dosage); i.LastWeek != "2026W46" || i.HitCount != 1 || i.ReopenCount != 1 {
		t.Fatalf("W47 dosage issue = %+v", i)
	}
	if tm := w.issue(timing); tm.FirstWeek != "2026W47" || tm.HitCount != 1 {
		t.Fatalf("W47 timing issue = %+v", tm)
	}
	if status(r4) != model.BackflowDrafted || status(bad) != model.BackflowFailed {
		t.Fatalf("W47 backflow r4=%q bad=%q", status(r4), status(bad))
	}

	// W48：DOSAGE 两次（行仍 OPEN）→ hit_count=2、reopen_count 不加；FAILED 的 bad 不占名额，r5、r6 都出草稿。
	w.dislike(r5, model.RejectDosage, tue(w45.AddDate(0, 0, 21)))
	w.dislike(r6, model.RejectDosage, tue(w45.AddDate(0, 0, 21)).Add(time.Hour))
	w.monday(w45.AddDate(0, 0, 21))
	if i = w.issue(dosage); i.LastWeek != "2026W48" || i.HitCount != 2 || i.ReopenCount != 1 || i.FirstWeek != "2026W45" {
		t.Fatalf("W48 issue = %+v", i)
	}
	if status(r5) != model.BackflowDrafted || status(r6) != model.BackflowDrafted || status(bad) != model.BackflowFailed {
		t.Fatalf("W48 backflow r5=%q r6=%q bad=%q", status(r5), status(r6), status(bad))
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM attribution_issue WHERE tenant_id = 0 AND farm_id = 0`); n != 2 {
		t.Fatalf("platform rows after 4 weeks = %d, want 2", n)
	}

	// RAW_READ：每次 ReadRaw 一行（服务账号），5 个出草稿的 run 各 1 行 OK，bad 3 次 DENIED。
	if w.raw.n != 8 {
		t.Fatalf("ReadRaw calls = %d, want 8", w.raw.n)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND farm_id = ? AND action = 'RAW_READ'
  AND actor_id = ?`, itest.TenantID, itest.FarmID, svcUser); n != int64(w.raw.n) {
		t.Fatalf("RAW_READ audit rows = %d, ReadRaw calls = %d", n, w.raw.n)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action = 'RAW_READ' AND actor_id = ? AND result = 'DENIED'
  AND run_id = ?`, svcUser, bad); n != model.BackflowMaxAttempts {
		t.Fatalf("bad RAW_READ DENIED rows = %d, want %d", n, model.BackflowMaxAttempts)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action = 'BACKFLOW' AND run_id = ?
  AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.status')) = 'RETRY'`, bad); n != model.BackflowMaxAttempts {
		t.Fatalf("bad RETRY events = %d, want %d", n, model.BackflowMaxAttempts)
	}
	entries, err := os.ReadDir(w.draftDir)
	if err != nil || len(entries) != 5 {
		t.Fatalf("drafts = %d, %v; want 5", len(entries), err)
	}
}
