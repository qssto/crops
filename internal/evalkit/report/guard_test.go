package report

import (
	"bytes"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
)

// 报告首行声明作废原因与阈值（应失败用例：未接入时首行仍是 Gate 0 结论）。
func TestRender_GuardFirstLine(t *testing.T) {
	cases := []struct {
		gate evalkit.GateStatus
		want []string
	}{
		{evalkit.GateStatus{Status: gateVoid, VoidReason: "INFRA_ERROR_RATE", Hits: []string{"INFRA_ERROR_RATE", "GRADER_ERROR"},
			MaxInfraErrorRate: 0.05}, []string{"VOID", "void_reason = INFRA_ERROR_RATE", "INFRA_ERROR_RATE, GRADER_ERROR", "阈值 5%"}},
		{evalkit.GateStatus{Status: gateBudgetExceeded, TokenBudget: 1000, MaxInfraErrorRate: 0.05},
			[]string{"BUDGET_EXCEEDED", "token_budget = 1000", "阈值 5%"}},
		{evalkit.GateStatus{Status: "DONE", MaxInfraErrorRate: 0.125}, []string{"作废判定：未命中", "阈值 12.5%"}},
	}
	for _, c := range cases {
		var b bytes.Buffer
		if err := Render(&b, &evalkit.Experiment{ID: "x", N: 1, Gate: c.gate}); err != nil {
			t.Fatal(err)
		}
		lines := strings.SplitN(b.String(), "\n", 4)
		if len(lines) < 3 {
			t.Fatalf("report = %q", b.String())
		}
		for _, w := range c.want {
			if !strings.Contains(lines[2], w) {
				t.Errorf("%s: first line %q lacks %q", c.gate.Status, lines[2], w)
			}
		}
	}
	var b bytes.Buffer
	if err := Render(&b, &evalkit.Experiment{ID: "x", N: 1}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "作废判定") {
		t.Fatal("undecided gate rendered a guard line")
	}
}
