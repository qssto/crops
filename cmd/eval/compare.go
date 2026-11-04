package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/store"
)

// knowledgeDir 是阶段级报告列变更文件的知识目录（相对 --repo）。
const knowledgeDir = "knowledge/"

// errUnreleasable 是 --fail-on-unreleasable 时结论不可放行的退出错误（L7 CI 据退出码非 0 拦截）。
var errUnreleasable = errors.New("compare: conclusion is not releasable")

func cmdCompare(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	var stf storeFlags
	stf.register(fs)
	baselineRef := fs.String("baseline", "", "baseline 名（{baselines-dir}/{名}.yaml 的 exp_id）或 experiment.id")
	candidate := fs.Uint64("candidate", 0, "候选 experiment.id")
	mode := fs.String("mode", string(compare.ModeFactor), "factor（单因子，写 factor_diff）| phase（阶段级，只出报告）")
	reference := fs.Uint64("reference", 0, "阶段级第三列 experiment.id（baseline 配置 + 新知识快照），拆知识侧 / agent 侧")
	seed := fs.Uint64("bootstrap-seed", compare.DefaultSeed, "配对 bootstrap seed（写入报告）")
	resamples := fs.Int("resamples", compare.DefaultResamples, "bootstrap 重抽次数")
	outRoot := fs.String("out", defaultOut, "报告输出根目录（写 {out}/compare/{baseline}-{candidate}.md）")
	baselinesDir := fs.String("baselines-dir", defaultBaselines, "baseline yaml 目录")
	repo := fs.String("repo", defaultRepo, "仓库根目录（阶段级列两边 git_sha 之间 knowledge/ 的变更）")
	failOnUnreleasable := fs.Bool("fail-on-unreleasable", false,
		"结论不可放行时退出码 1（报告照写；阶段级恒不可放行）；默认 false 只打印 releasable=false")
	if err := fs.Parse(args); err != nil {
		return err
	}
	m, err := compare.ParseMode(*mode)
	if err != nil {
		return err
	}
	if *baselineRef == "" || *candidate == 0 {
		return errors.New("compare: --baseline and --candidate are required")
	}
	if *reference != 0 && m != compare.ModePhase {
		return compare.ErrReferencePhaseOnly
	}
	es, err := stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	baseID, err := resolveBaseline(ctx, es.exps, *baselinesDir, *baselineRef)
	if err != nil {
		return err
	}

	in := compare.Input{Mode: m, Seed: *seed, Resamples: *resamples}
	if in.Baseline, err = loadSide(ctx, es.exps, baseID); err != nil {
		return err
	}
	if in.Candidate, err = loadSide(ctx, es.exps, *candidate); err != nil {
		return err
	}
	if *reference != 0 {
		ref, err := loadSide(ctx, es.exps, *reference)
		if err != nil {
			return err
		}
		in.Reference = &ref
	}
	keys, err := es.exps.PairedSamples(ctx, baseID, *candidate)
	if err != nil {
		return err
	}
	for _, k := range keys {
		in.Paired = append(in.Paired, k.String())
	}
	if m == compare.ModePhase {
		if in.KnowledgeChanges, err = knowledgeChanges(ctx, *repo, in.Baseline.Snapshot.GitSHA, in.Candidate.Snapshot.GitSHA); err != nil {
			return err
		}
	}
	r, err := compare.Run(in)
	if err != nil {
		if errors.Is(err, compare.ErrIncomparable) {
			return fmt.Errorf("%w（baseline %d / candidate %d）", err, baseID, *candidate)
		}
		return err
	}
	path, err := writeCompareReport(*outRoot, r)
	if err != nil {
		return err
	}
	if diff, ok, err := r.Record(); err != nil {
		return err
	} else if ok {
		if err := es.exps.RecordCompare(ctx, *candidate, baseID, store.CompareFactor, diff); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "compare done: mode=%s baseline=%d candidate=%d paired=%d excluded=%d overall=%s conclusion=%s releasable=%t report=%s\n",
		r.Mode, baseID, *candidate, r.Paired, len(r.Excluded), r.Overall, conclusionText(r), r.Releasable(), path)
	return releaseGate(r, *failOnUnreleasable)
}

// releaseGate 在 --fail-on-unreleasable 时把不可放行的结论变成错误（退出码 1）。
func releaseGate(r *compare.Result, failOnUnreleasable bool) error {
	if failOnUnreleasable && !r.Releasable() {
		return fmt.Errorf("%w: mode=%s conclusion=%s", errUnreleasable, r.Mode, conclusionText(r))
	}
	return nil
}

// loadSide 读实验状态、快照与全部 trial；没有快照（P1 未补录）的实验不能对比。DONE 与 trial 非空由 compare.Run / AA 校验。
func loadSide(ctx context.Context, exps *store.Experiments, id uint64) (compare.Side, error) {
	rec, err := exps.Get(ctx, id)
	if err != nil {
		return compare.Side{}, err
	}
	if rec.Snapshot == nil {
		return compare.Side{}, fmt.Errorf("%w: experiment %d（P1 实验先 baseline freeze --backfill）", compare.ErrNoSnapshot, id)
	}
	trials, err := exps.Trials(ctx, id)
	if err != nil {
		return compare.Side{}, err
	}
	return compare.Side{ID: id, Done: rec.Done(), Snapshot: *rec.Snapshot, Trials: trials}, nil
}

// knowledgeChanges 列两边 git_sha 之间 knowledge/ 下的变更文件（同一 sha 为空）。
func knowledgeChanges(ctx context.Context, repo, from, to string) ([]string, error) {
	if from == to {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--name-only", from, to, "--", knowledgeDir).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s %s -- %s: %w", from, to, knowledgeDir, err)
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

func writeCompareReport(outRoot string, r *compare.Result) (string, error) {
	dir := filepath.Join(outRoot, "compare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("compare: mkdir %s: %w", dir, err)
	}
	var b strings.Builder
	if err := compare.Render(&b, r); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%d-%d.md", r.BaselineID, r.CandidateID))
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("compare: write %s: %w", path, err)
	}
	return path, nil
}

// conclusionText 是 stdout 的结论列：阶段级没有门禁结论。
func conclusionText(r *compare.Result) string {
	if r.Conclusion == "" {
		return "-"
	}
	return string(r.Conclusion)
}
