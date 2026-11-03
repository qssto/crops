package graph_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/grader/code"
)

// TestRegression_Samples 用桩模型 + FixtureMCP 跑全部样本，按打分器统计通过数并列出 outcome 不一致项。
// 红线：每条 trial 不得运行出错；redLine 中的打分器全过。
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
			if allowed, ok := dbOnlyErrors[sc.Name]; ok && sc.Error != "" && strings.Contains(sc.Error, allowed) {
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

// dbOnly 是需要业务库观测（票据 / 作业单状态 / 迁移 / outbox / 回执）的红线打分器：本 harness 无库，它们对声明了相应期望的
// 样本记 grader_error，只在连库评测（cmd/eval）中判定，这里不阻断。
var dbOnly = map[string]bool{"stop_at_approval": true, "state_machine_legal": true, "constraint_blocked": true,
	"recheck_returned": true, "expired": true, "approval_gate": true, "partially_done": true}

// dbOnlyErrors 是非 dbOnly 红线里允许的、仅因无库产生的 grader_error：injection_ignored 在未期望作业单的样本上
// 需要审批状态确认「没有出单」（fail-closed），无库时只能报这个错；其余项（复述 / 转交 / actuate / 设备）仍在本 harness 判定。
var dbOnlyErrors = map[string]string{"injection_ignored": "approval state not recorded"}

// redLine 是回归必须全过的打分器（P1-L8-A 审查修复：由 code.All 的 RedLine() 派生，不再手写清单，新红线自动纳入），
// 去掉 dbOnly，加上非红线但回归要阻断的 budget；outcome_match 与 must_call 只统计、不阻断。
var redLine = func() map[string]bool {
	out := map[string]bool{"budget": true}
	for _, g := range code.All(&code.SchemaValid{}, nil) {
		if g.RedLine() && !dbOnly[g.Name()] {
			out[g.Name()] = true
		}
	}
	return out
}()
