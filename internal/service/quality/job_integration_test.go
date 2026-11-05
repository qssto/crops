//go:build integration

package quality_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service/quality"
	"agri-steward/internal/service/quality/judge"
)

// 合成一日（2026-11-03，Asia/Shanghai，UTC 窗口 [11-02 16:00, 11-03 16:00)），经 AdviceRepo.SaveAdvice 落卡。
var (
	itNow   = time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	itInDay = time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	itDate  = "2026-11-03"
)

const itVersion = "itest0123456789-standard"

type itClock struct{}

func (itClock) Now() time.Time { return itNow }

// itJudge 全给 3 分，evidence 取结论原句。
type itJudge struct{ calls int }

func (j *itJudge) Judge(_ context.Context, _ judge.Prompt, m judge.Material) (judge.Verdict, error) {
	j.calls++
	three := 3
	return judge.Verdict{Score: &three, Evidence: m.Output.Conclusion}, nil
}

type itKB struct{}

func (itKB) Open(context.Context, string) (judge.EntryLoader, error) { return itKB{}, nil }

func (itKB) Load(_ context.Context, kbID, version string) (agent.KnowledgeEntry, error) {
	return agent.KnowledgeEntry{KBID: kbID, Version: version, Body: json.RawMessage(`{"text":"合成知识条目"}`)}, nil
}

type itObjects struct{ m map[string][]byte }

func (o *itObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	b, ok := o.m[bucket+"/"+key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func (o *itObjects) PutObject(_ context.Context, bucket, key string, body []byte, _ string) error {
	o.m[bucket+"/"+key] = body
	return nil
}

type itCard struct {
	farm    uint64
	cat     model.Category
	refuse  string // 非空为拒答
	fullCit bool   // 结论与理由都有引用
	at      time.Time
}

type seeded struct{ advice, run uint64 }

// seedCards 建 agent_run（knowledge_ref=kb@test）并经 AdviceRepo.SaveAdvice 落卡（created_at 取 FrozenClock）。
func seedCards(t *testing.T, e *itest.Env, cards []itCard) (*env.Slot, []seeded) {
	t.Helper()
	slot := e.Fresh(t, itInDay)
	sqlDB, err := slot.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	advice := repository.NewAdviceRepo(slot.DB, slot.Clock)
	out := make([]seeded, 0, len(cards))
	for i, c := range cards {
		res, err := sqlDB.ExecContext(t.Context(), `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status,
  graph_version, prompt_version, model_tier, knowledge_ref, created_at, updated_at)
  VALUES (?, ?, 1, 1001, 'RUNNING', 'g1', 'p1', 'standard', 'kb@test', ?, ?)`, itest.TenantID, c.farm, c.at, c.at)
		if err != nil {
			t.Fatal(err)
		}
		runID, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		scope := model.Scope{TenantID: itest.TenantID, FarmID: c.farm}
		card := model.AdviceCard{RunID: uint64(runID), TenantID: itest.TenantID, FarmID: c.farm, FieldID: 1, Crop: "rice",
			PhenologyStage: "分蘖末期", Category: c.cat, Conclusion: fmt.Sprintf("建议对第 %d 号地块按规则层剂量作业", i+1),
			Reasons: []string{"虫口密度达到防治指标"}}
		if c.refuse != "" {
			card.Outcome, card.RefuseReason = model.OutcomeRefused, c.refuse
		} else {
			card.Outcome, card.AdviceType, card.NextAction = model.OutcomeAdvice, model.AdviceTypeOperation, model.NextCreateOrder
			cite := func(path string) model.Citation {
				return model.Citation{TenantID: itest.TenantID, FarmID: c.farm, Type: model.CitationKB, ClaimPath: path,
					KBID: "rice/pest/planthopper", KBVersion: "v1"}
			}
			card.Citations = []model.Citation{cite("")}
			if c.fullCit {
				card.Citations = append(card.Citations, cite("reasons[0]"))
			}
		}
		slot.Clock.Set(c.at)
		id, err := advice.SaveAdvice(t.Context(), scope, card)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, seeded{advice: id, run: uint64(runID)})
	}
	return slot, out
}

// itDay：农场 1 出卡 protect 4（3 张全引用）、fertilize 2（全引用），VALIDATION_FAILED 1、OUT_OF_SCOPE 1，
// 另有一张 11-02 15:59:59Z 的卡（属于前一日，必须排除）；农场 2 出卡 protect 2（全引用）、VALIDATION_FAILED 1。
func itDay() []itCard {
	p, f := model.CategoryProtect, model.CategoryFertilize
	return []itCard{
		{farm: 1, cat: p, fullCit: true, at: itInDay}, {farm: 1, cat: p, fullCit: true, at: itInDay},
		{farm: 1, cat: p, fullCit: true, at: itInDay}, {farm: 1, cat: p, at: itInDay},
		{farm: 1, cat: f, fullCit: true, at: itInDay}, {farm: 1, cat: f, fullCit: true, at: itInDay},
		{farm: 1, cat: p, refuse: quality.RefuseValidation, at: itInDay},
		{farm: 1, cat: p, refuse: "OUT_OF_SCOPE", at: itInDay},
		{farm: 1, cat: p, fullCit: true, at: time.Date(2026, 11, 2, 15, 59, 59, 0, time.UTC)},
		{farm: 2, cat: p, fullCit: true, at: itInDay}, {farm: 2, cat: p, fullCit: true, at: itInDay},
		{farm: 2, cat: p, refuse: quality.RefuseValidation, at: itInDay},
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func itJob(t *testing.T, e *itest.Env, slot *env.Slot, j judge.Client, obj *itObjects) (*quality.Job, *quality.RecordingNotifier) {
	t.Helper()
	store := repository.QualityRepo{DB: slot.DB}
	sampler := &quality.Sampler{Store: store, Cards: repository.NewAdviceRepo(slot.DB, slot.Clock), KB: itKB{}, Judge: j,
		Version: itVersion, Tokens: repository.NewTokenBucket(e.Redis), ModelKey: slot.Keys.Raw("rl:model:standard"),
		JudgeKey: slot.Keys.RlJudge("standard"), ModelQPM: 60, JudgeQPM: 60, Quota: 2, Salt: "p2l3", Clock: itClock{},
		Logger: quiet()}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	n := &quality.RecordingNotifier{}
	job, err := quality.NewJob(quality.JobDeps{Farms: store, Rollup: quality.Rollup{Store: store, Version: itVersion},
		Sampler: sampler, Reports: store, Objects: obj, Bucket: "eval", Notifier: n, Locks: repository.NewLocker(e.Redis),
		Keys: slot.Keys, Clock: itClock{}, Logger: quiet(), ReportLoc: loc, BaselineName: "baseline_p1.r2"})
	if err != nil {
		t.Fatal(err)
	}
	return job, n
}

type dailyRow struct {
	TenantID, FarmID       uint64
	Metric                 string
	Numerator, Denominator int
	Point                  float64
	IsEstimate             bool
}

func dailyRows(t *testing.T, db *repository.DB) map[string]dailyRow {
	t.Helper()
	var rows []dailyRow
	if err := db.WithContext(t.Context()).Raw(`SELECT tenant_id, farm_id, metric, numerator, denominator, point, is_estimate
FROM quality_metric_daily WHERE stat_date = ? AND source = 'ONLINE' AND slice_key = 'all'`, itDate).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	out := make(map[string]dailyRow, len(rows))
	for _, r := range rows {
		out[fmt.Sprintf("%d/%d/%s", r.TenantID, r.FarmID, r.Metric)] = r
	}
	return out
}

// P2-L3 验收 2（人工对账，合成数据）：农场行与平台行逐项对手算值；补跑同键覆盖、取值不变、judge 不重复调用。
func TestQualityJob_RollupRerun_Integration(t *testing.T) {
	e := itest.Setup(t, "quality")
	slot, _ := seedCards(t, e, itDay())
	j := &itJudge{}
	obj := &itObjects{m: map[string][]byte{}}
	job, n := itJob(t, e, slot, j, obj)
	out, err := job.RunOnce(t.Context())
	if err != nil || !out.Ran || out.Date != itDate {
		t.Fatalf("RunOnce = %+v, %v", out, err)
	}
	// 手算：农场 1 引用 5/6、数值 6/7、拒答 2/8；农场 2 引用 2/2、数值 2/3、拒答 1/3；平台行为两者之和；
	// 抽样每类 2 张：农场 1 protect 2 + fertilize 2、农场 2 protect 2，judge 全 3 分 → 事实一致 6/6（抽样估计）。
	cc, na, rr, fc := model.MetricCitationCoverage, model.MetricNumericAccuracy, model.MetricRefusalRate, model.MetricFactConsistency
	want := map[string][2]int{
		"1/1/" + cc: {5, 6}, "1/1/" + na: {6, 7}, "1/1/" + rr: {2, 8}, "1/1/" + fc: {4, 4},
		"1/2/" + cc: {2, 2}, "1/2/" + na: {2, 3}, "1/2/" + rr: {1, 3}, "1/2/" + fc: {2, 2},
		"0/0/" + cc: {7, 8}, "0/0/" + na: {8, 10}, "0/0/" + rr: {3, 11}, "0/0/" + fc: {6, 6},
	}
	got := dailyRows(t, slot.DB)
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %d", got, len(want))
	}
	for k, w := range want {
		r := got[k]
		if r.Numerator != w[0] || r.Denominator != w[1] || r.IsEstimate != strings.HasSuffix(k, fc) {
			t.Fatalf("%s = %+v, want %v", k, r, w)
		}
	}
	if j.calls != 6 {
		t.Fatalf("judge calls = %d, want 6", j.calls)
	}
	if _, ok := obj.m["eval/report/daily/20261103.html"]; !ok || len(n.Sent()) != 1 {
		t.Fatalf("report / notify missing: %v %v", obj.m, n.Sent())
	}
	// 补跑（新进程）：同键覆盖，行数与取值不变，judge 不重复调用。
	job2, _ := itJob(t, e, slot, j, obj)
	if _, err := job2.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	again := dailyRows(t, slot.DB)
	for k, r := range got {
		if again[k] != r {
			t.Fatalf("rerun changed %s: %+v -> %+v", k, r, again[k])
		}
	}
	if total := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM quality_metric_daily`); total != int64(len(want)) {
		t.Fatalf("rerun rows = %d, want %d", total, len(want))
	}
	if c := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM online_judge_result`); j.calls != 6 || c != 6 {
		t.Fatalf("rerun judged again: calls %d, results %d", j.calls, c)
	}
	if c := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM run_event`); c != 0 {
		t.Fatalf("rollup wrote run_event: %d", c)
	}
}

// 抽样可复现：同盐同数据两次抽到同一批卡，且顺序等于 Go 侧按 CRC32(CONCAT(id, salt)) 升序（MySQL CRC32 即 IEEE）。
func TestQualitySampleAdvice_Reproducible_Integration(t *testing.T) {
	e := itest.Setup(t, "quality")
	slot, cards := seedCards(t, e, itDay())
	store := repository.QualityRepo{DB: slot.DB}
	day, err := quality.FarmDay(itNow, "Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: 1}
	ids := func(salt string) []uint64 {
		refs, err := store.SampleAdvice(t.Context(), scope, string(model.CategoryProtect), day.From, day.To, salt, 2)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]uint64, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.ID)
		}
		return out
	}
	for _, salt := range []string{"p2l3", "other-salt"} {
		first, second := ids(salt), ids(salt)
		// 农场 1 当日 protect 出卡是 cards[0..3]（cards[8] 在窗口外）。
		pool := []uint64{cards[0].advice, cards[1].advice, cards[2].advice, cards[3].advice}
		slices.SortFunc(pool, func(a, b uint64) int {
			ca, cb := crc32.ChecksumIEEE([]byte(fmt.Sprint(a, salt))), crc32.ChecksumIEEE([]byte(fmt.Sprint(b, salt)))
			return int(int64(ca) - int64(cb))
		})
		if !slices.Equal(first, second) || !slices.Equal(first, pool[:2]) {
			t.Fatalf("salt %s: sample %v / %v, want %v", salt, first, second, pool[:2])
		}
	}
}

// 线上 judge 结果：uk_advice_dim 重复不覆盖；专家回写乐观锁，旧 row_ver 冲突返回 ErrJudgeRowConflict，且回写后专家分计入。
func TestQualityJudgeResult_DuplicateAndReview_Integration(t *testing.T) {
	e := itest.Setup(t, "quality")
	slot, cards := seedCards(t, e, itDay())
	store := repository.QualityRepo{DB: slot.DB}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: 1}
	row := model.OnlineJudgeResult{TenantID: itest.TenantID, FarmID: 1, RunID: cards[0].run, AdviceID: cards[0].advice,
		StatDate: itDate, Category: string(model.CategoryProtect), Dimension: judge.DimFactConsistency, JudgeVersion: itVersion,
		Status: model.JudgeStatusNeedExpert}
	for i, want := range []bool{true, false} {
		ok, err := store.InsertJudgeResult(t.Context(), scope, row, itNow)
		if err != nil || ok != want {
			t.Fatalf("insert #%d = %v, %v, want %v", i, ok, err, want)
		}
	}
	got, err := store.JudgeResults(t.Context(), scope, itDate, judge.DimFactConsistency)
	if err != nil || len(got) != 1 || got[0].JudgeScore != nil || got[0].RowVer != 0 {
		t.Fatalf("JudgeResults = %+v, %v", got, err)
	}
	id := got[0].ID
	r := quality.Reviewer{Store: store, Clock: itClock{}}
	if err := r.Review(t.Context(), scope, id, 0, 3, 1002); err != nil {
		t.Fatal(err)
	}
	if err := r.Review(t.Context(), scope, id, 0, 2, 1004); !errors.Is(err, repository.ErrJudgeRowConflict) {
		t.Fatalf("stale row_ver err = %v, want ErrJudgeRowConflict", err)
	}
	if err := r.Review(t.Context(), model.Scope{TenantID: itest.TenantID, FarmID: 2}, id, 1, 2, 2001); !errors.Is(err, repository.ErrJudgeRowConflict) {
		t.Fatalf("cross-farm review err = %v, want ErrJudgeRowConflict", err)
	}
	tally, err := store.FarmJudgeTally(t.Context(), scope, itDate, judge.DimFactConsistency, itVersion, 3)
	if err != nil || tally.Scored != 1 || tally.Top != 1 || tally.Unknown != 0 {
		t.Fatalf("tally after review = %+v, %v (expert score must count)", tally, err)
	}
}

// rl:judge 令牌桶：容量用完即拒，按 QPM 匀速补充（注入时间）。
func TestQualityTokenBucket_Integration(t *testing.T) {
	e := itest.Setup(t, "quality")
	slot := e.Fresh(t, itNow)
	b := repository.NewTokenBucket(e.Redis)
	key := slot.Keys.RlJudge("standard")
	ctx := context.Background()
	for i, want := range []bool{true, true, false} {
		ok, err := b.Take(ctx, key, 2, itNow)
		if err != nil || ok != want {
			t.Fatalf("take #%d = %v, %v, want %v", i, ok, err, want)
		}
	}
	// 2/min：30s 补一个。
	if ok, err := b.Take(ctx, key, 2, itNow.Add(30*time.Second)); err != nil || !ok {
		t.Fatalf("take after 30s = %v, %v", ok, err)
	}
	if ok, err := b.Take(ctx, key, 2, itNow.Add(30*time.Second)); err != nil || ok {
		t.Fatalf("second take after 30s = %v, %v, want denied", ok, err)
	}
	// 退还一个后可再取一个；退还不超过容量（满桶退还后仍只能取 2 个）。
	if err := b.Refund(ctx, key, 2, itNow.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Take(ctx, key, 2, itNow.Add(30*time.Second)); err != nil || !ok {
		t.Fatalf("take after refund = %v, %v", ok, err)
	}
	full := slot.Keys.Raw("rl:model:standard")
	if err := b.Refund(ctx, full, 2, itNow); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, true, false} {
		if ok, err := b.Take(ctx, full, 2, itNow); err != nil || ok != want {
			t.Fatalf("take after refund on full bucket #%d = %v, %v, want %v", i, ok, err, want)
		}
	}
}
