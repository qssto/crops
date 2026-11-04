package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/smoke"
	"agri-steward/internal/evalkit/store"
)

// defaultSmokeList 是 PR 冒烟子集清单（P2 §6.8 L7-a 任务 1）。
const defaultSmokeList = "eval/smoke/smoke.yaml"

// suiteHoldout 是保留集套件名（L2 以 submodule 挂到 eval/suites/holdout/）。
const suiteHoldout = "holdout"

// run --mode 取值（make eval MODE=...，P2 §6.8 技术实现 1）。
const (
	modeSmoke   = "smoke"
	modeNightly = "nightly"
	modeRelease = "release"
)

var (
	// errModeConflict 表示 --mode 与 --suite / --n / --tags / --trigger 同时给出（预设不许被覆盖）。
	errModeConflict = errors.New("--mode fixes suite / n / tags / trigger; do not pass them")
	// errBadMode 表示 --mode 不在 runModes 内。
	errBadMode = errors.New("--mode must be smoke|nightly|release")
	// errHoldoutMissing 表示 release 要求的 holdout 套件未挂载（失败关闭）。
	errHoldoutMissing = errors.New("release needs the holdout suite: no sample with suite: holdout under --suites-dir " +
		"(mount it: git submodule update --init eval/suites/holdout)")
)

// runMode 是一种触发的预设：套件、次数、trigger、是否只跑冒烟清单；allSuites 时套件取全部已加载样本的套件。
type runMode struct {
	suites    []string
	allSuites bool
	n         int
	trigger   string
	smokeList bool
}

// runModes 是 --mode 注册表：smoke = 冒烟子集 n=3；nightly = regression+redteam+capability n=5
// （n 不得小于 store.GraduateTrials，否则 graduate 永远 0 个可毕业）；release = 全部套件含 holdout n=5。
var runModes = map[string]runMode{
	modeSmoke:   {suites: smoke.Suites, n: 3, trigger: store.TriggerPR, smokeList: true},
	modeNightly: {suites: []string{"regression", "redteam", "capability"}, n: 5, trigger: store.TriggerNightly},
	modeRelease: {allSuites: true, n: 5, trigger: store.TriggerRelease},
}

// presetFlags 是 --mode 不许同时给出的参数（--smoke-list 是清单路径，可与 --mode smoke 同时给出）。
var presetFlags = []string{"suite", "n", "tags", "trigger"}

// checkModeFlags 在 --mode 非空时拒绝同时显式给出的预设参数。
func checkModeFlags(fs *flag.FlagSet, mode string) error {
	if mode == "" {
		return nil
	}
	if _, ok := runModes[mode]; !ok {
		return fmt.Errorf("%w: %q", errBadMode, mode)
	}
	var set []string
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains(presetFlags, f.Name) {
			set = append(set, "--"+f.Name)
		}
	})
	if len(set) > 0 {
		return fmt.Errorf("%w: %s", errModeConflict, strings.Join(set, " "))
	}
	return nil
}

// suitesOf 返回 m 要跑的套件：allSuites 时取 samples 中全部套件（排序），且必须含 holdout。
func (m runMode) suitesOf(samples []*sample.Sample) ([]string, error) {
	if !m.allSuites {
		return slices.Clone(m.suites), nil
	}
	var all []string
	for _, s := range samples {
		if !slices.Contains(all, s.Suite) {
			all = append(all, s.Suite)
		}
	}
	slices.Sort(all)
	if !slices.Contains(all, suiteHoldout) {
		return nil, errHoldoutMissing
	}
	return all, nil
}

// selectRun 按 --mode 选样本并定运行参数：mode 为空时按 opts.Suites / opts.Tags 筛选（L7-a 前行为）；
// 否则 suites / n 取预设、tags 清空，smoke 只取冒烟清单（先过覆盖校验），release 取全部套件且须含 holdout。
func selectRun(mode, smokePath string, ls *loadedSamples, opts evalkit.Options) ([]*sample.Sample, evalkit.Options, error) {
	if mode == "" {
		return evalkit.Select(ls.samples, opts.Suites, opts.Tags), opts, nil
	}
	m := runModes[mode]
	suites, err := m.suitesOf(ls.samples)
	if err != nil {
		return nil, opts, err
	}
	opts.Suites, opts.Tags, opts.N = suites, nil, m.n
	if !m.smokeList {
		return evalkit.Select(ls.samples, opts.Suites, nil), opts, nil
	}
	selected, err := selectSmoke(smokePath, ls)
	return selected, opts, err
}

// selectSmoke 读冒烟清单并按覆盖规则校验（不合格失败关闭，不跑），返回清单内样本（保持加载顺序）。
func selectSmoke(path string, ls *loadedSamples) ([]*sample.Sample, error) {
	l, err := smoke.Load(path)
	if err != nil {
		return nil, err
	}
	if err := smoke.Check(l, ls.samples, ls.graders); err != nil {
		return nil, err
	}
	ids := l.IDs()
	var out []*sample.Sample
	for _, s := range ls.samples {
		if slices.Contains(ids, s.ID) {
			out = append(out, s)
		}
	}
	return out, nil
}
