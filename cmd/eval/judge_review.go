package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/store"
)

const (
	// calibrationKeyPrefix / recheckKeyPrefix 是 eval bucket 里一致性报告的对象键前缀。
	calibrationKeyPrefix = "report/calibration"
	recheckKeyPrefix     = "report/recheck"
	// chinaOffsetSec 是月度复核切月用的北京时间偏移（不依赖系统 tzdata）。
	chinaOffsetSec = 8 * 3600
	// noteReplay：eval/judges/judge.yaml 目前只接受录制回放模型。
	noteReplay = "待真实模型：judge 为录制回放（eval.judge.model=replay），一致性结论不代表真实模型"
	// noteNoExperts：没有 DONE 的专家主标任务时 Kappa 无定义。
	noteNoExperts = "待专家：尚无专家打分（DONE 主标任务为 0）"
)

// errNoJudgeVersion 是实验快照没有该维度的 judge 版本（未用 run --judges 跑）。
var errNoJudgeVersion = errors.New("judge: experiment snapshot has no judge version for dimension (eval run --judges first)")

// reviewFlags 是 calibrate / recheck 共用参数。
type reviewFlags struct {
	stf       storeFlags
	dim       string
	exp       uint64
	gate      string
	resamples int
	out       string
}

func (f *reviewFlags) register(fs *flag.FlagSet) {
	f.stf.register(fs)
	fs.StringVar(&f.dim, "dim", "", "judge 维度 "+strings.Join(judge.Dimensions, "|"))
	fs.Uint64Var(&f.exp, "exp", 0, "experiment.id（快照 judge_versions 须含该维度）")
	fs.StringVar(&f.gate, "gate", review.DefaultGate, "judge 门禁配置（kappa_threshold 与各维度开关）")
	fs.IntVar(&f.resamples, "resamples", review.DefaultResamples, "Kappa bootstrap 重抽次数")
	fs.StringVar(&f.out, "out", defaultOut, "本地报告根目录")
}

func (f *reviewFlags) parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.exp == 0 {
		return fmt.Errorf("%s: --exp is required", fs.Name())
	}
	return judge.CheckDim(f.dim)
}

// loadCandidates 取实验快照中该维度的 judge 版本与可进专家任务的 trial（holdout 已排除）。
func loadCandidates(ctx context.Context, exps *store.Experiments, exp uint64, dim string) (string, []review.Candidate, error) {
	rec, err := exps.Get(ctx, exp)
	if err != nil {
		return "", nil, err
	}
	if rec.Snapshot == nil || rec.Snapshot.JudgeVersions[dim] == "" {
		return "", nil, fmt.Errorf("%w: experiment %d dim %s", errNoJudgeVersion, exp, dim)
	}
	trials, err := exps.Trials(ctx, exp)
	if err != nil {
		return "", nil, err
	}
	return rec.Snapshot.JudgeVersions[dim], review.Candidates(trials, dim), nil
}

// cmdJudgeCalibrate 分层抽样写 CALIBRATION 任务（重跑幂等），再按已完成的专家打分出一致性报告：
// MinIO report/calibration/{dim}/{judge_version}.json + 本地 {out}/calibration/{dim}/{judge_version}.md。
func cmdJudgeCalibrate(ctx context.Context, args []string, stdout, stderr io.Writer, clk clock.Clock) (err error) {
	fs := flag.NewFlagSet("judge calibrate", flag.ContinueOnError)
	var rf reviewFlags
	rf.register(fs)
	total := fs.Int("total", review.DefaultCalibrationTotal, "主标任务数（不足时报告列出）")
	seed := fs.Uint64("seed", review.DefaultSeed, "分层抽样 / 双标 / bootstrap seed（写入报告）")
	if err := rf.parse(fs, args); err != nil {
		return err
	}
	gate, err := review.LoadGate(rf.gate)
	if err != nil {
		return err
	}
	es, err := rf.stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	version, cands, err := loadCandidates(ctx, es.exps, rf.exp, rf.dim)
	if err != nil {
		return err
	}
	plan := review.PlanCalibration(rf.exp, rf.dim, version, cands, review.SampleOptions{Total: *total, Seed: *seed})
	reviews := store.NewReviews(es.db)
	now := clk.Now()
	inserted, err := reviews.Insert(ctx, plan.Tasks, now)
	if err != nil {
		return err
	}
	sc := store.StatsScope{TaskType: store.TaskCalibration, Dimension: rf.dim, JudgeVersion: version}
	r, err := agreement(ctx, reviews, sc, rf, gate, *seed, now)
	if err != nil {
		return err
	}
	key := path.Join(calibrationKeyPrefix, rf.dim, version+".json")
	md, err := publishReport(ctx, es, r, key, filepath.Join(rf.out, "calibration", rf.dim), version+".md")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "judge calibrate: dim=%s exp=%d judge_version=%s primary=%d second=%d inserted=%d seed=%d plan_short=%q n=%d undefined=%d kappa=%s gate_ready=%t gate_reason=%q report=%s/%s md=%s\n",
		rf.dim, rf.exp, version, plan.Primary, plan.Second, inserted, *seed, plan.Short, r.JudgeExpert.N, r.JudgeExpert.Undefined,
		kappaText(r.JudgeExpert), r.GateReady, r.GateReason, es.bucket, key, md)
	return nil
}

// cmdJudgeRecheck 每月（北京时间切月）对一个维度抽 size 个 trial 写 MONTHLY_RECHECK 任务：本月已抽过则不再抽
// （按 created_at 落在本月窗口计），seed 为 YYYYMM；再按已完成的复核打分出一致性报告：
// MinIO report/recheck/{dim}/{YYYY-MM}.json + 本地 {out}/recheck/{dim}/{YYYY-MM}.md。时钟注入（测月份边界）。
func cmdJudgeRecheck(ctx context.Context, args []string, stdout, stderr io.Writer, clk clock.Clock) (err error) {
	fs := flag.NewFlagSet("judge recheck", flag.ContinueOnError)
	var rf reviewFlags
	rf.register(fs)
	size := fs.Int("size", review.DefaultRecheckSize, "每月复核 trial 数")
	if err := rf.parse(fs, args); err != nil {
		return err
	}
	gate, err := review.LoadGate(rf.gate)
	if err != nil {
		return err
	}
	es, err := rf.stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	version, cands, err := loadCandidates(ctx, es.exps, rf.exp, rf.dim)
	if err != nil {
		return err
	}
	cst := time.FixedZone("CST", chinaOffsetSec)
	now := clk.Now()
	from, to := review.MonthWindow(now, cst)
	month, seed := review.MonthTag(now, cst), review.MonthSeed(now, cst)
	reviews := store.NewReviews(es.db)
	existing, err := reviews.CountCreated(ctx, store.TaskMonthlyRecheck, rf.dim, from, to)
	if err != nil {
		return err
	}
	inserted := 0
	if existing == 0 {
		tasks := review.PlanRecheck(rf.exp, rf.dim, version, cands, *size, seed)
		if inserted, err = reviews.Insert(ctx, tasks, now); err != nil {
			return err
		}
	}
	// 只统计本月创建的复核任务（报告按月命名，L2 评审 P2-8）。
	sc := store.StatsScope{TaskType: store.TaskMonthlyRecheck, Dimension: rf.dim, JudgeVersion: version, From: from, To: to}
	r, err := agreement(ctx, reviews, sc, rf, gate, seed, now)
	if err != nil {
		return err
	}
	key := path.Join(recheckKeyPrefix, rf.dim, month+".json")
	md, err := publishReport(ctx, es, r, key, filepath.Join(rf.out, "recheck", rf.dim), month+".md")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "judge recheck: dim=%s exp=%d month=%s judge_version=%s already_planned=%d inserted=%d seed=%d n=%d undefined=%d kappa=%s report=%s/%s md=%s\n",
		rf.dim, rf.exp, month, version, existing, inserted, seed, r.JudgeExpert.N, r.JudgeExpert.Undefined, kappaText(r.JudgeExpert), es.bucket, key, md)
	return nil
}

// agreement 跑三条统计查询（judge-专家、专家间、UNKNOWN 占比）并出报告。
// sc 限定任务类型、维度、judge 版本，月度复核另限本月创建的任务。
func agreement(ctx context.Context, reviews *store.Reviews, sc store.StatsScope, rf reviewFlags,
	gate *review.Gate, seed uint64, now time.Time) (review.Report, error) {
	je, err := reviews.JudgeExpertMatrix(ctx, sc)
	if err != nil {
		return review.Report{}, err
	}
	ie, err := reviews.ExpertMatrix(ctx, sc)
	if err != nil {
		return review.Report{}, err
	}
	ratio, ok, err := reviews.UnknownRatio(ctx, sc)
	if err != nil {
		return review.Report{}, err
	}
	notes := []string{noteReplay}
	if len(je) == 0 {
		notes = append(notes, noteNoExperts)
	}
	return review.BuildReport(review.ReportInput{TaskType: sc.TaskType, Dimension: sc.Dimension, JudgeVersion: sc.JudgeVersion,
		JudgeExpert: je, InterExpert: ie, UnknownRatio: ratio, UnknownOK: ok, Gate: gate,
		Resamples: rf.resamples, Seed: seed, Now: now, Notes: notes}), nil
}

// publishReport 把报告 JSON 上传 eval bucket 的 key，并在本地 dir/name 写 md，返回本地路径。
func publishReport(ctx context.Context, es *evalStore, r review.Report, key, dir, name string) (string, error) {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("judge: marshal report: %w", err)
	}
	if err := es.objects.PutObject(ctx, es.bucket, key, body, "application/json"); err != nil {
		return "", fmt.Errorf("judge: upload %s: %w", key, err)
	}
	var b strings.Builder
	if err := r.Markdown(&b); err != nil {
		return "", err
	}
	return writeLocal(dir, name, b.String())
}

// kappaText 是 stdout 的 Kappa 列：κ [下限, 上限]，无定义为 undefined。
func kappaText(iv review.Interval) string {
	if !iv.OK {
		return "undefined"
	}
	return fmt.Sprintf("%.3f[%.3f,%.3f]", iv.Kappa, iv.Low, iv.High)
}
