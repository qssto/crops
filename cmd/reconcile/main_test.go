package main

import (
	"strings"
	"testing"

	"agri-steward/internal/service"
)

func TestCompare(t *testing.T) {
	m := service.DayMetrics{Sessions: 3, Completion: service.Ratio{Num: 2, Den: 3},
		ToolSuccess: service.Ratio{Num: 4, Den: 5}, Tokens: service.TokenStats{Total: 160},
		Jobs: service.JobMetrics{DryRunExcluded: 2}}
	d := direct{runs: pair{3, 2}, tools: pair{5, 4}, tokens: pair{Total: 160}, orders: pair{Total: 0, Hit: 2}}
	var b strings.Builder
	if !printRows(&b, compare(m, d)) {
		t.Fatalf("expected all equal:\n%s", b.String())
	}
	d.tools.Hit = 3
	b.Reset()
	if printRows(&b, compare(m, d)) || !strings.Contains(b.String(), "| 工具成功 | `tool_success_rate.num` | 4 | 3 | **不一致** |") {
		t.Fatalf("mismatch not reported:\n%s", b.String())
	}
}
