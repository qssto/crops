package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/store"
)

const (
	// defaultThresholds 是 A/A 阈值草稿（入库，Owner 签字后进门禁）。
	defaultThresholds = "eval/gates/thresholds.yaml"
	// aaSuite / aaN 是不带 --from-exps 时 A/A 的套件与每样本次数（P2 §6.1 任务 6：regression 全量 n=5）。
	aaSuite = "regression"
	aaN     = 5
)

// thresholdsHeader 写在阈值草稿 yaml 顶部。
const thresholdsHeader = "# A/A 噪声阈值草稿（P2 §6.1 功能任务 6）：同一快照多次运行，指标两两配对差值分布的 95% 分位为阈值下限。\n" +
	"# 由 `go run ./cmd/eval aa` 生成，勿手改数值；signed_by 留空待 Owner 签字后进门禁。\n"

func cmdAA(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("aa", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	var inf infraFlags
	inf.register(fs)
	runs := fs.Int("runs", 3, "不带 --from-exps 时同一快照运行次数")
	fromExps := fs.String("from-exps", "", "直接用已有实验，逗号分隔 experiment.id（须同一快照）")
	out := fs.String("out", defaultThresholds, "阈值草稿输出路径")
	seed := fs.Uint64("bootstrap-seed", compare.DefaultSeed, "bootstrap seed")
	resamples := fs.Int("resamples", compare.DefaultResamples, "bootstrap 重抽次数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ids, err := parseIDs(*fromExps)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		if *runs < compare.MinAARuns {
			return fmt.Errorf("aa: --runs must be >= %d, got %d", compare.MinAARuns, *runs)
		}
		if ids, err = runAA(ctx, &sf, &inf, *runs, stdout, stderr); err != nil {
			return err
		}
	} else if len(ids) < compare.MinAARuns {
		return fmt.Errorf("aa: --from-exps needs >= %d experiments, got %d", compare.MinAARuns, len(ids))
	}
	stf := storeFlags{configFile: inf.configFile, envFile: inf.envFile}
	es, err := stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	sides := make([]compare.Side, 0, len(ids))
	for _, id := range ids {
		s, err := loadSide(ctx, es.exps, id)
		if err != nil {
			return err
		}
		sides = append(sides, s)
	}
	th, err := compare.AA(sides, *seed, *resamples, clock.System{}.Now())
	if err != nil {
		return err
	}
	if err := writeThresholds(*out, th); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "aa done: experiments=%v metrics=%d out=%s signed_by=(待 Owner)\n", th.Experiments, len(th.Metrics), *out)
	return nil
}

// runAA 在 regression 全量上用同一快照跑 runs 次 n=5，返回各次 experiment.id。
func runAA(ctx context.Context, sf *sampleFlags, inf *infraFlags, runs int, stdout, stderr io.Writer) (_ []uint64, err error) {
	ls, err := sf.loadValidSamples()
	if err != nil {
		return nil, err
	}
	opts := evalkit.Options{Suites: []string{aaSuite}, N: aaN}
	selected := evalkit.Select(ls.samples, opts.Suites, nil)
	if len(selected) == 0 {
		return nil, fmt.Errorf("aa: no sample in suite %s", aaSuite)
	}
	in, err := inf.open(ctx, stderr, ls)
	if err != nil {
		return nil, fmt.Errorf("eval infra unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, in.close()) }()
	ids := make([]uint64, 0, runs)
	for i := range runs {
		exp, id, err := runWithStore(ctx, in, ls, selected, opts, store.TriggerManual)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(stdout, "aa run %d/%d: exp_id=%s trials=%d\n", i+1, runs, exp.ID, len(exp.Trials))
		ids = append(ids, id)
	}
	return ids, nil
}

// parseIDs 解析 --from-exps；重复 id 报错（同一实验与自己配对差值恒为 0，会把噪声阈值压低）。
func parseIDs(s string) ([]uint64, error) {
	var ids []uint64
	seen := map[uint64]struct{}{}
	for _, p := range splitList(s) {
		id, err := strconv.ParseUint(p, 10, 64)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("aa: bad experiment id %q", p)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: %d", compare.ErrAADuplicate, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func writeThresholds(path string, th *compare.Thresholds) error {
	body, err := yaml.Marshal(th)
	if err != nil {
		return fmt.Errorf("aa: marshal thresholds: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("aa: mkdir: %w", err)
	}
	if err := os.WriteFile(path, append([]byte(thresholdsHeader), body...), 0o644); err != nil {
		return fmt.Errorf("aa: write %s: %w", path, err)
	}
	return nil
}
