package graph_test

import (
	"fmt"
	"sort"
	"testing"
)

// TestRegression_Samples 用桩模型 + FixtureMCP 跑全部样本，按打分器统计通过数并列出 outcome 不一致项。
// 红线：每条 trial 不得运行出错；schema / citation_valid / numbers_match_calc / product_in_candidates / budget 全过。
func TestRegression_Samples(t *testing.T) {
	h := newHarness(t)
	ss := loadSamples(t)
	if len(ss) < 20 {
		t.Fatalf("want >= 20 samples, got %d", len(ss))
	}
	cats := map[string]int{}
	for _, s := range ss {
		if a := s.Expect.Advice; a != nil {
			cats[a.Category]++
		}
	}
	t.Logf("samples=%d advice-by-category=%v", len(ss), cats)
	for _, c := range []string{"protect", "fertilize", "irrigate", "disaster"} {
		if cats[c] == 0 {
			t.Errorf("no advice sample for category %s", c)
		}
	}

	exp := h.runExperiment(t, ss)
	pass, total := map[string]int{}, map[string]int{}
	var mismatches, redFails []string
	for _, tr := range exp.Trials {
		if tr.Error != "" {
			t.Errorf("%s: run error: %s", tr.SampleID, tr.Error)
			continue
		}
		for _, sc := range tr.Scores {
			if sc.Unknown {
				continue
			}
			total[sc.Name]++
			if sc.Pass {
				pass[sc.Name]++
				continue
			}
			line := fmt.Sprintf("%s %s: %s %s", tr.SampleID, sc.Name, sc.Evidence, sc.Error)
			if sc.Name == "outcome_match" {
				mismatches = append(mismatches, line)
			} else if redLine[sc.Name] {
				redFails = append(redFails, line)
			}
		}
	}
	names := make([]string, 0, len(total))
	for n := range total {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Logf("grader %-22s %3d/%3d", n, pass[n], total[n])
	}
	for _, m := range mismatches {
		t.Logf("MISMATCH %s", m)
	}
	for _, f := range redFails {
		t.Errorf("RED %s", f)
	}
}

// redLine 是回归必须全过的打分器（outcome_match 与 must_call 只统计、不阻断）。
var redLine = map[string]bool{"schema_valid": true, "citation_valid": true, "numbers_match_calc": true,
	"product_in_candidates": true, "budget": true}
