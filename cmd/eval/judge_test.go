package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/review"
)

func TestCmdJudge_Usage(t *testing.T) {
	ctx := context.Background()
	var out, errOut bytes.Buffer
	for name, args := range map[string][]string{"none": nil, "unknown": {"label"}} {
		if err := cmdJudge(ctx, args, &out, &errOut); !errors.Is(err, errJudgeUsage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// 参数校验先于连库：未知维度 / 缺 --exp 不需要 MySQL。
	if err := cmdJudge(ctx, []string{"calibrate", "--dim", "x", "--exp", "1"}, &out, &errOut); !errors.Is(err, judge.ErrUnknownDim) {
		t.Errorf("unknown dim: err = %v", err)
	}
	if err := cmdJudge(ctx, []string{"recheck", "--dim", judge.DimFactConsistency}, &out, &errOut); err == nil {
		t.Error("recheck without --exp accepted")
	}
}

// --judges 默认关：打分器与 judge_versions（空表）不变；打开后追加三个 judge 并记版本，不改原切片。
func TestJudgeFlags_Attach(t *testing.T) {
	base := &loadedSamples{}
	var off judgeFlags
	if err := off.attach(base); err != nil || len(base.graders) != 0 {
		t.Fatalf("disabled attach: graders %d err %v", len(base.graders), err)
	}
	if m := base.judgeVersionMap(); m == nil || len(m) != 0 {
		t.Fatalf("judge_versions without --judges = %v", m)
	}
	on := judgeFlags{enabled: true, dir: filepath.Join(repoRoot, judge.DefaultDir), config: filepath.Join(repoRoot, judge.DefaultConfig),
		gate: filepath.Join(repoRoot, review.DefaultGate)}
	ls := &loadedSamples{}
	if err := on.attach(ls); err != nil {
		t.Fatal(err)
	}
	if len(ls.graders) != len(judge.Dimensions) || len(ls.judgeVersionMap()) != len(judge.Dimensions) {
		t.Fatalf("graders %d versions %v", len(ls.graders), ls.judgeVersions)
	}
	for _, g := range ls.graders {
		if g.RedLine() || ls.judgeVersions[g.Name()] == "" {
			t.Errorf("judge %s red_line=%t version=%q", g.Name(), g.RedLine(), ls.judgeVersions[g.Name()])
		}
	}
}

// L2 评审裁决：--judges 按门禁配置决定 judge 出错是否算 grader_error：仓库默认三维度全关 → 全部 advisory；
// 打开 fact_consistency → 该维度出错照旧 grader_error。
func TestJudgeFlags_AttachAdvisoryByGate(t *testing.T) {
	on := judgeFlags{enabled: true, dir: filepath.Join(repoRoot, judge.DefaultDir), config: filepath.Join(repoRoot, judge.DefaultConfig),
		gate: filepath.Join(repoRoot, review.DefaultGate)}
	enabled := filepath.Join(t.TempDir(), "judges.yaml")
	cfg := "kappa_threshold: 0.6\ndimensions:\n  fact_consistency:\n    enabled: true\n  conclusion_match:\n    enabled: false\n  explanation_quality:\n    enabled: false\n"
	if err := os.WriteFile(enabled, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	for gate, wantGated := range map[string]string{on.gate: "", enabled: judge.DimFactConsistency} {
		f := on
		f.gate = gate
		ls := &loadedSamples{}
		if err := f.attach(ls); err != nil {
			t.Fatal(err)
		}
		for _, g := range ls.graders {
			adv, ok := g.(grader.Advisory)
			if want := g.Name() != wantGated; !ok || adv.Advisory() != want {
				t.Errorf("gate %s: %s advisory = %v, want %v", filepath.Base(gate), g.Name(), ok && adv.Advisory(), want)
			}
		}
	}
	bad := on
	bad.gate = filepath.Join(t.TempDir(), "missing.yaml")
	if err := bad.attach(&loadedSamples{}); err == nil {
		t.Fatal("missing gate config accepted")
	}
}
