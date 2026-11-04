package report

import (
	"fmt"
	"strings"

	"agri-steward/internal/evalkit"
)

// 门禁终态取值（与 store.StatusVoid / StatusBudgetExceeded 一致；report 不依赖 store）。
const (
	gateVoid           = "VOID"
	gateBudgetExceeded = "BUDGET_EXCEEDED"
)

// GuardLine 是报告首行的门禁终态声明（P2 L7-a）：VOID 写作废原因与全部命中规则，BUDGET_EXCEEDED 写预算，
// 均声明「结论不可用」；其余写作废判定未命中。阈值一律写出。Gate 未判定（单测、旧调用）返回空串，不输出该行。
func GuardLine(g evalkit.GateStatus) string {
	threshold := fmt.Sprintf("infra_error 占比阈值 %s（> 即作废）、grader_error > 0 即作废", fmtRate(g.MaxInfraErrorRate))
	switch g.Status {
	case "":
		return ""
	case gateVoid:
		return fmt.Sprintf("**实验作废 VOID：void_reason = %s**（命中 %s；%s）。结论不可用，compare / baseline freeze 拒绝本实验",
			g.VoidReason, strings.Join(g.Hits, ", "), threshold)
	case gateBudgetExceeded:
		return fmt.Sprintf("**预算超限 BUDGET_EXCEEDED**：累计 token 超过 token_budget = %d，已停止派发新 trial（%s）。"+
			"结论不可用，compare / baseline freeze 拒绝本实验", g.TokenBudget, threshold)
	}
	return "作废判定：未命中（" + threshold + "）"
}

// fmtRate 把占比格式化为百分数（0.05 → 5%）。
func fmtRate(r float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", r*100), "0"), ".") + "%"
}
