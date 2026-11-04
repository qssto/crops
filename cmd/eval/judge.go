package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/store"
)

// errJudgeUsage 是 eval judge 子命令缺失或未知。
var errJudgeUsage = errors.New("usage: eval judge <queue|calibrate|recheck> [flags]")

// judgeFlags 是 run --judges：在样本静态校验之后追加三个 LLM-judge 打分器（RedLine=false，门禁默认关、只进报告），
// 并把 judge 版本写进快照 judge_versions。默认不开：快照 judge_versions 仍为空表，与 P1 / 现有 baseline 可比。
type judgeFlags struct {
	enabled bool
	dir     string
	config  string
	gate    string
}

func (f *judgeFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.enabled, "judges", false, "追加 LLM-judge 打分器（只进报告，不进门禁），版本写快照 judge_versions")
	fs.StringVar(&f.dir, "judges-dir", judge.DefaultDir, "judge 提示词 / 评分标准 / 录制根目录")
	fs.StringVar(&f.config, "judge-config", judge.DefaultConfig, "judge 模型配置（eval.judge.model / model_tier）")
	fs.StringVar(&f.gate, "judge-gate", review.DefaultGate, "judge 门禁配置：enabled 的维度出错记 grader_error，未开的只记 judge_error、不改 trial 状态")
}

// attach 在 loadValidSamples 之后调用（参考解校验不经 judge）：追加 judge 打分器并记录版本。
func (f *judgeFlags) attach(ls *loadedSamples) error {
	if !f.enabled {
		return nil
	}
	cfg, err := judge.LoadConfig(f.config)
	if err != nil {
		return err
	}
	gate, err := review.LoadGate(f.gate)
	if err != nil {
		return err
	}
	client, err := judge.LoadReplay(f.dir)
	if err != nil {
		return err
	}
	gs, err := judge.All(f.dir, client, func(dim string) bool { return gate.Dimensions[dim].Enabled })
	if err != nil {
		return err
	}
	versions, err := judge.Versions(f.dir, cfg.ModelTier)
	if err != nil {
		return err
	}
	ls.graders = append(slices.Clip(ls.graders), gs...)
	ls.judgeVersions = versions
	return nil
}

// judgeVersionMap 是快照 judge_versions：未开 --judges 为空表（非 nil，Missing 把 nil 当未填）。
func (ls *loadedSamples) judgeVersionMap() map[string]string {
	if ls.judgeVersions == nil {
		return map[string]string{}
	}
	return ls.judgeVersions
}

// cmdJudge 分派 judge 子命令：queue（人工队列入队）、calibrate（校准抽样 + 一致性报告）、recheck（月度复核）。
func cmdJudge(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errJudgeUsage
	}
	switch args[0] {
	case "queue":
		return cmdJudgeQueue(ctx, args[1:], stdout, stderr, clock.System{})
	case "calibrate":
		return cmdJudgeCalibrate(ctx, args[1:], stdout, stderr, clock.System{})
	case "recheck":
		return cmdJudgeRecheck(ctx, args[1:], stdout, stderr, clock.System{})
	}
	return fmt.Errorf("%w: unknown subcommand %q", errJudgeUsage, args[0])
}

// cmdJudgeQueue 把一个实验的 trial 按四类入口（CONFLICT / UNKNOWN / FLAKY / RANDOM_10）写 HUMAN_QUEUE 任务；
// 重跑幂等（ref_key 唯一，ON DUPLICATE KEY UPDATE id = id）。seed 写进本地报告。
func cmdJudgeQueue(ctx context.Context, args []string, stdout, stderr io.Writer, clk clock.Clock) (err error) {
	fs := flag.NewFlagSet("judge queue", flag.ContinueOnError)
	var stf storeFlags
	stf.register(fs)
	exp := fs.Uint64("exp", 0, "experiment.id")
	seed := fs.Uint64("seed", review.DefaultSeed, "RANDOM_10 抽检 seed（写入报告）")
	outRoot := fs.String("out", defaultOut, "报告输出根目录（写 {out}/review/queue-{exp}.md）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *exp == 0 {
		return errors.New("judge queue: --exp is required")
	}
	es, err := stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	rec, err := es.exps.Get(ctx, *exp)
	if err != nil {
		return err
	}
	trials, err := es.exps.Trials(ctx, *exp)
	if err != nil {
		return err
	}
	in := review.QueueInput{ExperimentID: *exp, Trials: trials, Seed: *seed}
	if rec.Snapshot != nil {
		in.JudgeVersions = rec.Snapshot.JudgeVersions
	}
	tasks, counts := review.Queue(in, review.Rules)
	inserted, err := store.NewReviews(es.db).Insert(ctx, tasks, clk.Now())
	if err != nil {
		return err
	}
	path, err := writeQueueReport(*outRoot, *exp, *seed, counts, len(tasks), inserted)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "judge queue: exp=%d tasks=%d inserted=%d %s seed=%d report=%s\n",
		*exp, len(tasks), inserted, countsText(counts), *seed, path)
	return nil
}

// countsText 按注册表顺序列各入口命中数。
func countsText(counts map[string]int) string {
	parts := make([]string, 0, len(review.Rules))
	for _, r := range review.Rules {
		parts = append(parts, fmt.Sprintf("%s=%d", r.Reason(), counts[r.Reason()]))
	}
	return strings.Join(parts, " ")
}

func writeQueueReport(outRoot string, exp, seed uint64, counts map[string]int, total, inserted int) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# 人工队列 exp %d\n\n- RANDOM_10 seed: %d\n- 任务: %d（本次新写 %d，其余已在队列）\n\n| 入口 | 任务数 |\n|---|---|\n",
		exp, seed, total, inserted)
	for _, r := range review.Rules {
		fmt.Fprintf(&b, "| %s | %d |\n", r.Reason(), counts[r.Reason()])
	}
	return writeLocal(filepath.Join(outRoot, "review"), fmt.Sprintf("queue-%d.md", exp), b.String())
}

// writeLocal 写本地报告文件，返回路径。
func writeLocal(dir, name, body string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("judge: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("judge: write %s: %w", path, err)
	}
	return path, nil
}
