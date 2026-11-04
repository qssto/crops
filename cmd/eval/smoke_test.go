package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/smoke"
)

// TestSmokeList 校验入库的冒烟清单：样本都存在且可运行、samples 都在 regression、
// 覆盖 run 所用打分器里全部红线打分器（红线集合由 RedLine() 派生）。
func TestSmokeList(t *testing.T) {
	sf := sampleFlags{dir: filepath.Join(repoRoot, defaultDir), taxonomy: filepath.Join(repoRoot, defaultTax), repo: repoRoot}
	ls, err := sf.loadValidSamples()
	if err != nil {
		t.Fatal(err)
	}
	l, err := smoke.Load(filepath.Join(repoRoot, defaultSmokeList))
	if err != nil {
		t.Fatal(err)
	}
	if err := smoke.Check(l, ls.samples, ls.graders); err != nil {
		t.Fatal(err)
	}
	if n := len(l.IDs()); n < 25 || n > 35 {
		t.Fatalf("smoke list has %d samples, want about 30", n)
	}
	if len(smoke.RedLines(ls.graders)) == 0 {
		t.Fatal("no red-line grader derived")
	}
}

// 适用按样本声明判定：删掉全部声明 expect.constraint 的样本（池与清单同删）后，constraint_blocked 无适用样本，
// Check 必须失败；不能因打分器对未声明样本返回 Pass 就算适用。
func TestSmokeList_DropConstraintFails(t *testing.T) {
	sf := sampleFlags{dir: filepath.Join(repoRoot, defaultDir), taxonomy: filepath.Join(repoRoot, defaultTax), repo: repoRoot}
	ls, err := sf.loadValidSamples()
	if err != nil {
		t.Fatal(err)
	}
	l, err := smoke.Load(filepath.Join(repoRoot, defaultSmokeList))
	if err != nil {
		t.Fatal(err)
	}
	var dropped []string
	pool := slices.DeleteFunc(slices.Clone(ls.samples), func(s *sample.Sample) bool {
		if s.Expect.Constraint != nil {
			dropped = append(dropped, s.ID)
			return true
		}
		return false
	})
	if len(dropped) == 0 {
		t.Fatal("no sample declares expect.constraint")
	}
	l.Samples = slices.DeleteFunc(l.Samples, func(id string) bool { return slices.Contains(dropped, id) })
	l.Supplement = slices.DeleteFunc(l.Supplement, func(s smoke.Supplement) bool { return slices.Contains(dropped, s.ID) })
	err = smoke.Check(l, pool, ls.graders)
	if !errors.Is(err, smoke.ErrCoverage) || !strings.Contains(err.Error(), "red-line grader constraint_blocked") {
		t.Fatalf("err = %v, want constraint_blocked under-covered", err)
	}
}
