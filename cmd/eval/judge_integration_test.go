//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

// fixedClock 是注入时钟（月份边界）。
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// judgeExp 直接经 store 建一个带 judge_versions 的 DONE 实验与 n 个 trial：fact_consistency 的 judge 分数按 i 轮转
// 1/2/3/UNKNOWN（1 分少、3 分多），citation_valid 与 judge 结论一致（不触发 CONFLICT）。没有真实 judge 模型（待真实模型）。
func judgeExp(t *testing.T, exps *store.Experiments, version string, n int) uint64 {
	t.Helper()
	ctx := context.Background()
	snap := evalkit.Snapshot{GitSHA: strings.Repeat("a", 40), PromptVersion: "p", ModelTier: "t", Model: "stub",
		KnowledgeRef: "kc:it", SamplesHash: "h", LoaderStrategy: evalkit.LoaderV0, RetrievalStrategy: evalkit.RetrievalProgressive,
		JudgeVersions: map[string]string{judge.DimFactConsistency: version}, Suites: []string{"regression"}, Tags: []string{}, N: 1}
	start := time.Now().UTC().Truncate(time.Millisecond)
	id, err := exps.Start(ctx, store.TriggerManual, snap, start)
	if err != nil {
		t.Fatal(err)
	}
	exp := &evalkit.Experiment{ID: store.FormatID(id), StartedAt: start, FinishedAt: start.Add(time.Second), Snapshot: snap}
	for i := range n {
		s := []int{1, 2, 3, 3, 3, 0, 2, 3, 3, 3}[i%10]
		js := evalkit.Score{Name: judge.DimFactConsistency, Pass: true, Value: float64(s), Unknown: s == 0}
		cv := evalkit.Score{Name: "citation_valid", RedLine: true, Pass: s != 1, Value: 1}
		exp.Trials = append(exp.Trials, evalkit.Trial{SampleID: "it-" + strconv.Itoa(i), SampleVersion: 1,
			Suite: []string{"regression", "capability", "constraint"}[i%3], Scores: []evalkit.Score{cv, js}})
	}
	if err := exps.Finish(ctx, id, exp); err != nil {
		t.Fatal(err)
	}
	return id
}

// itVersion 是本轮唯一的 judge 版本（隔离库跨轮次复用，统计查询按版本隔开）。
func itVersion() string { return "it-" + strconv.FormatInt(time.Now().UnixNano(), 36) }

func judgeArgs(sub, dim string, exp uint64, out string) []string {
	return append([]string{"judge", sub, "--dim", dim, "--exp", store.FormatID(exp), "--out", out,
		"--gate", filepath.Join(repoRoot, review.DefaultGate)}, storeArgs()...)
}

func field(t *testing.T, out, name string) string {
	t.Helper()
	m := regexp.MustCompile(` ` + name + `=(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("stdout missing %s: %s", name, out)
	}
	return m[1]
}

// scoreAll 合成专家打分（待专家）：主标给 expert 1001、双标给 1002；专家分取 judge 分（UNKNOWN 取 2），
// 双标与主标同分。先验证双标不能派给主标专家。
func scoreAll(t *testing.T, reviews *store.Reviews, taskType, version string) {
	t.Helper()
	ctx := context.Background()
	tasks, err := reviews.List(ctx, store.ReviewFilter{TaskType: taskType, Dimension: judge.DimFactConsistency, JudgeVersion: version})
	if err != nil || len(tasks) == 0 {
		t.Fatalf("list tasks = %d err %v", len(tasks), err)
	}
	const primary, second = 1001, 1002
	for _, task := range tasks {
		who := uint64(primary)
		if task.RaterNo == store.RaterSecond {
			if err := reviews.Assign(ctx, task.ID, primary); !errors.Is(err, store.ErrSameRater) {
				t.Fatalf("second rater assigned to primary expert: err = %v", err)
			}
			who = second
		}
		if err := reviews.Assign(ctx, task.ID, who); err != nil {
			t.Fatal(err)
		}
		score := 2
		if task.JudgeScore != nil {
			score = *task.JudgeScore
		}
		if err := reviews.Score(ctx, task.ID, who, score, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestJudgeCalibrate_Integration 走真库与 MinIO：分层抽样写 188 个 CALIBRATION 任务（主标 150 + 双标 38），重跑不重复（ON DUPLICATE KEY）；
// 没有专家分时 Kappa 无定义、标「待专家」；合成专家分后 Kappa 区间下限 ≥ 0.6，报告标「可进门禁」，JSON 在
// report/calibration/{dim}/{judge_version}.json，本地 md 同名。
func TestJudgeCalibrate_Integration(t *testing.T) {
	ctx := context.Background()
	db, cfg := evalDB(t)
	exps, reviews := store.New(db), store.NewReviews(db)
	version := itVersion()
	exp := judgeExp(t, exps, version, 300)
	out := t.TempDir()
	// --total 150：主标含 UNKNOWN 层，排除后 judge-专家评分对仍 ≥ review.MinCalibrationPairs。
	args := append(judgeArgs("calibrate", judge.DimFactConsistency, exp, out), "--total", "150")

	stdout, errOut, code := cli(t, args...)
	if code != exitOK || field(t, stdout, "primary") != "150" || field(t, stdout, "second") != "38" ||
		field(t, stdout, "inserted") != "188" || field(t, stdout, "kappa") != "undefined" || field(t, stdout, "gate_ready") != "false" {
		t.Fatalf("first calibrate code=%d stdout=%s stderr=%s", code, stdout, errOut)
	}
	countSQL := "SELECT COUNT(*) FROM review_task WHERE task_type = ? AND judge_version = ?"
	if n := countRows(t, db, countSQL, store.TaskCalibration, version); n != 188 {
		t.Fatalf("calibration tasks = %d", n)
	}
	if stdout, errOut, code = cli(t, args...); code != exitOK || field(t, stdout, "inserted") != "0" {
		t.Fatalf("rerun code=%d stdout=%s stderr=%s", code, stdout, errOut)
	}
	if n := countRows(t, db, countSQL, store.TaskCalibration, version); n != 188 {
		t.Fatalf("rerun duplicated tasks: %d", n)
	}

	scoreAll(t, reviews, store.TaskCalibration, version)
	if stdout, errOut, code = cli(t, args...); code != exitOK || field(t, stdout, "gate_ready") != "true" ||
		!strings.HasPrefix(field(t, stdout, "kappa"), "1.000[1.000,") {
		t.Fatalf("scored calibrate code=%d stdout=%s stderr=%s", code, stdout, errOut)
	}
	md, err := os.ReadFile(filepath.Join(out, "calibration", judge.DimFactConsistency, version+".md"))
	if err != nil || !bytes.Contains(md, []byte("可进门禁")) || !bytes.Contains(md, []byte("待真实模型")) {
		t.Fatalf("local md err=%v:\n%s", err, md)
	}
	mc, err := app.NewMinIO(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := path.Join(calibrationKeyPrefix, judge.DimFactConsistency, version+".json")
	body, err := repository.MinIOGetter{Client: mc}.GetObject(ctx, cfg.MinIO.BucketEval, key)
	if err != nil {
		t.Fatal(err)
	}
	var r review.Report
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if !r.GateReady || r.Enabled || !r.InterExpert.OK || r.InterExpert.Kappa != 1 || r.UnknownRatio == nil || len(r.Short) != 0 ||
		r.JudgeExpert.N < review.MinCalibrationPairs || !r.JudgeExpert.Usable {
		t.Fatalf("report = %+v", r)
	}
}

// TestJudgeRecheck_Integration 用注入时钟测月度复核：同一（北京时间）月第二次不再抽；UTC 月末 16:00 后已属下月。
// 隔离库跨轮次复用，月份取远期的随机年份，避免与上一轮同月。
func TestJudgeRecheck_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	exps := store.New(db)
	version := itVersion()
	// macOS 时钟只到微秒，取微秒数取模。
	year := 3000 + int(time.Now().UnixMicro()%6000)
	recheck := func(exp uint64, now time.Time) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if err := cmdJudgeRecheck(ctx, judgeArgs("recheck", judge.DimFactConsistency, exp, t.TempDir())[2:], &stdout, &stderr, fixedClock(now)); err != nil {
			t.Fatalf("recheck: %v stderr=%s", err, stderr.String())
		}
		return stdout.String()
	}
	dec := judgeExp(t, exps, version, 60)
	// UTC 11-30 16:30 = 北京时间 12-01 00:30：属 12 月。
	out := recheck(dec, time.Date(year, 11, 30, 16, 30, 0, 0, time.UTC))
	if field(t, out, "month") != strconv.Itoa(year)+"-12" || field(t, out, "inserted") != "20" || field(t, out, "already_planned") != "0" {
		t.Fatalf("december: %s", out)
	}
	if out = recheck(dec, time.Date(year, 12, 31, 15, 0, 0, 0, time.UTC)); field(t, out, "inserted") != "0" || field(t, out, "already_planned") != "20" {
		t.Fatalf("december again: %s", out)
	}
	// UTC 11-30 15:59 = 北京时间 11-30 23:59：仍属 11 月（另一个实验，见 TODO：同实验跨月 ref_key 相同）。
	nov := judgeExp(t, exps, version, 60)
	if out = recheck(nov, time.Date(year, 11, 30, 15, 59, 0, 0, time.UTC)); field(t, out, "month") != strconv.Itoa(year)+"-11" || field(t, out, "inserted") != "20" {
		t.Fatalf("november: %s", out)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM review_task WHERE task_type = ? AND judge_version = ? AND rater_no = 1",
		store.TaskMonthlyRecheck, version); n != 40 {
		t.Fatalf("monthly recheck tasks = %d", n)
	}
	// L2 评审 P2-8：报告标哪个月就只统计那个月创建的复核任务（两个月共 40 个，各月 n ≤ 20）。
	scoreAll(t, store.NewReviews(db), store.TaskMonthlyRecheck, version)
	monthN := func(exp uint64, now time.Time) int {
		t.Helper()
		n, err := strconv.Atoi(field(t, recheck(exp, now), "n"))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	decN, novN := monthN(dec, time.Date(year, 12, 15, 0, 0, 0, 0, time.UTC)), monthN(nov, time.Date(year, 11, 15, 0, 0, 0, 0, time.UTC))
	if decN == 0 || decN > 20 || novN == 0 || novN > 20 {
		t.Fatalf("per-month n: december %d, november %d; want each in (0, 20]", decN, novN)
	}
}

// TestJudgeQueue_Integration：入队写 HUMAN_QUEUE（UNKNOWN 30、RANDOM_10 取 270 个通过 trial 的 27 个），重跑不重复。
func TestJudgeQueue_Integration(t *testing.T) {
	db, _ := evalDB(t)
	exp := judgeExp(t, store.New(db), itVersion(), 300)
	args := append([]string{"judge", "queue", "--exp", store.FormatID(exp), "--out", t.TempDir()}, storeArgs()...)
	stdout, errOut, code := cli(t, args...)
	if code != exitOK || field(t, stdout, "UNKNOWN") != "30" || field(t, stdout, "RANDOM_10") != "27" ||
		field(t, stdout, "CONFLICT") != "0" || field(t, stdout, "inserted") != "57" || field(t, stdout, "seed") != strconv.Itoa(review.DefaultSeed) {
		t.Fatalf("queue code=%d stdout=%s stderr=%s", code, stdout, errOut)
	}
	if stdout, errOut, code = cli(t, args...); code != exitOK || field(t, stdout, "inserted") != "0" {
		t.Fatalf("rerun code=%d stdout=%s stderr=%s", code, stdout, errOut)
	}
	if n := countRows(t, db, "SELECT COUNT(*) FROM review_task WHERE task_type = ? AND experiment_id = ?", store.TaskHumanQueue, exp); n != 57 {
		t.Fatalf("human queue tasks = %d", n)
	}
}

// L2 评审 P1-4：同一 trial 的主标与双标不得同一专家，两个方向都拦（先派双标再派主标也拦）；
// 专家间矩阵排除同一专家的配对（绕过 Assign 直接改库的行也不进 Kappa）。
func TestReviewAssign_SameRaterBothWays_Integration(t *testing.T) {
	ctx := context.Background()
	db, _ := evalDB(t)
	reviews := store.NewReviews(db)
	version := itVersion()
	exp := judgeExp(t, store.New(db), version, 3)
	var tasks []store.ReviewTask
	for i := range 3 {
		score := 2
		for _, rater := range []int{store.RaterPrimary, store.RaterSecond} {
			tasks = append(tasks, store.ReviewTask{TaskType: store.TaskCalibration, ExperimentID: &exp, SampleID: fmt.Sprintf("s-%d", i),
				TrialIndex: new(int), Dimension: judge.DimFactConsistency, JudgeVersion: version, JudgeScore: &score, RaterNo: rater})
		}
	}
	if n, err := reviews.Insert(ctx, tasks, time.Now().UTC()); err != nil || n != len(tasks) {
		t.Fatalf("insert = %d, %v", n, err)
	}
	got, err := reviews.List(ctx, store.ReviewFilter{TaskType: store.TaskCalibration, JudgeVersion: version})
	if err != nil || len(got) != len(tasks) {
		t.Fatalf("list = %d, %v", len(got), err)
	}
	ids := map[string]uint64{} // sample#rater -> id
	for _, task := range got {
		ids[fmt.Sprintf("%s#%d", task.SampleID, task.RaterNo)] = task.ID
	}
	const x, y = 2001, 2002
	// s-0：先派双标给 x，再派主标给 x → 拒绝。
	if err := reviews.Assign(ctx, ids["s-0#2"], x); err != nil {
		t.Fatal(err)
	}
	if err := reviews.Assign(ctx, ids["s-0#1"], x); !errors.Is(err, store.ErrSameRater) {
		t.Fatalf("primary assigned to the second rater: err = %v", err)
	}
	// s-1：先派主标给 x，再派双标给 x → 拒绝；改派 y 可以。
	if err := reviews.Assign(ctx, ids["s-1#1"], x); err != nil {
		t.Fatal(err)
	}
	if err := reviews.Assign(ctx, ids["s-1#2"], x); !errors.Is(err, store.ErrSameRater) {
		t.Fatalf("second assigned to the primary rater: err = %v", err)
	}
	if err := reviews.Assign(ctx, ids["s-1#2"], y); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"s-1#1", "s-1#2"} {
		who := uint64(x)
		if strings.HasSuffix(k, "#2") {
			who = y
		}
		if err := reviews.Score(ctx, ids[k], who, 2, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// s-2：绕过 Assign 直接写成同一专家且 DONE（旧数据 / 手工改库），不得进专家间矩阵。
	if err := db.Exec("UPDATE review_task SET assignee_id = ?, status = 'DONE', expert_score = 3 WHERE id IN (?, ?)",
		x, ids["s-2#1"], ids["s-2#2"]).Error; err != nil {
		t.Fatal(err)
	}
	m, err := reviews.ExpertMatrix(ctx, store.StatsScope{TaskType: store.TaskCalibration, Dimension: judge.DimFactConsistency, JudgeVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 || m[0] != (store.ScorePair{A: 2, B: 2, Count: 1}) {
		t.Fatalf("expert matrix = %+v, want only s-1 (2,2)", m)
	}
}
