package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// 直接 SQL（与 docs/metrics.md「对账用的直接 SQL」一致）：参数为 tenant、farm、[from, to)。
const (
	sqlRuns = `SELECT COUNT(*) AS total, COALESCE(SUM(status='COMPLETED'),0) AS hit FROM agent_run
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`
	sqlTools = `SELECT COUNT(*) AS total, COALESCE(SUM(status='OK'),0) AS hit FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`
	sqlAdvice = `SELECT COUNT(*) AS total, COALESCE(SUM(outcome='REFUSED'),0) AS hit FROM advice_card
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`
	sqlRunsWithEvent = `SELECT COUNT(DISTINCT e.run_id) AS total, 0 AS hit FROM run_event e
 JOIN agent_run r ON r.id=e.run_id AND r.tenant_id=e.tenant_id AND r.farm_id=e.farm_id
 WHERE e.tenant_id=? AND e.farm_id=? AND e.event_type=? AND r.created_at>=? AND r.created_at<?`
	sqlTokens = `SELECT COALESCE(SUM(CAST(JSON_EXTRACT(e.payload,'$.prompt_tokens') AS SIGNED)
  + CAST(JSON_EXTRACT(e.payload,'$.completion_tokens') AS SIGNED)),0) AS total, 0 AS hit FROM run_event e
 JOIN agent_run r ON r.id=e.run_id AND r.tenant_id=e.tenant_id AND r.farm_id=e.farm_id
 WHERE e.tenant_id=? AND e.farm_id=? AND e.event_type='LLM_CALL_END' AND r.created_at>=? AND r.created_at<?`
	sqlTickets = `SELECT COUNT(*) AS total, 0 AS hit FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND ticket_kind='ORDER' AND status IN ('APPROVED','REJECTED')
   AND decided_at>=? AND decided_at<?`
	sqlOrders = `SELECT COALESCE(SUM(dispatch_mode='REAL'),0) AS total, COALESCE(SUM(dispatch_mode<>'REAL'),0) AS hit
 FROM work_order WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`
)

// pair 是一条直接 SQL 的结果：total 为计数 / 求和，hit 为条件计数。
type pair struct {
	Total int64
	Hit   int64
}

// direct 是直接 SQL 得到的一天的原始量。
type direct struct {
	runs, tools, advice, tickets pair
	validationRuns, resumedRuns  pair
	tokens                       pair
	orders                       pair // Total = REAL 单数，Hit = 非 REAL（DRY_RUN）单数
}

func queryDirect(ctx context.Context, db *repository.DB, scope model.Scope, from, to time.Time) (direct, error) {
	var d direct
	win := []any{scope.TenantID, scope.FarmID, from, to}
	evWin := func(typ model.EventType) []any {
		return []any{scope.TenantID, scope.FarmID, string(typ), from, to}
	}
	queries := []struct {
		name string
		q    string
		args []any
		dst  *pair
	}{
		{"agent_run", sqlRuns, win, &d.runs},
		{"tool_call", sqlTools, win, &d.tools},
		{"advice_card", sqlAdvice, win, &d.advice},
		{"validation", sqlRunsWithEvent, evWin(model.EventValidationFailed), &d.validationRuns},
		{"resumed", sqlRunsWithEvent, evWin(model.EventRunResumed), &d.resumedRuns},
		{"tokens", sqlTokens, win, &d.tokens},
		{"approval_ticket", sqlTickets, win, &d.tickets},
		{"work_order", sqlOrders, win, &d.orders},
	}
	for _, q := range queries {
		if err := db.WithContext(ctx).Raw(q.q, q.args...).Scan(q.dst).Error; err != nil {
			return direct{}, fmt.Errorf("direct %s: %w", q.name, err)
		}
	}
	return d, nil
}

// row 是对账表一行。
type row struct {
	item          string
	metric, sql   int64
	metricsSource string
}

// compare 把 Daily 的一天与直接 SQL 逐项对齐（纯函数）。
func compare(m service.DayMetrics, d direct) []row {
	return []row{
		{"会话数", int64(m.Sessions), d.runs.Total, "sessions"},
		{"完成会话", int64(m.Completion.Num), d.runs.Hit, "completion_rate.num"},
		{"工具调用", int64(m.ToolSuccess.Den), d.tools.Total, "tool_success_rate.den"},
		{"工具成功", int64(m.ToolSuccess.Num), d.tools.Hit, "tool_success_rate.num"},
		{"建议卡", int64(m.Refusal.Den), d.advice.Total, "refusal_rate.den"},
		{"拒答", int64(m.Refusal.Num), d.advice.Hit, "refusal_rate.num"},
		{"校验失败 run", int64(m.ValidationFailure.Num), d.validationRuns.Total, "validation_failure_rate.num"},
		{"恢复过的 run", int64(m.Resume.Den), d.resumedRuns.Total, "resume_success_rate.den"},
		{"token 合计", int64(m.Tokens.Total), d.tokens.Total, "tokens.total"},
		{"已决策票", int64(m.ApprovalWait.Decided), d.tickets.Total, "approval_wait.decided"},
		{"REAL 作业单", int64(m.Jobs.Orders), d.orders.Total, "jobs.orders"},
		{"DRY_RUN 排除", int64(m.Jobs.DryRunExcluded), d.orders.Hit, "jobs.dry_run_excluded"},
	}
}

// printRows 打印 Markdown 表，返回是否全部一致。
func printRows(w io.Writer, rows []row) bool {
	ok := true
	fmt.Fprintln(w, "| 项 | 指标字段 | MetricsService | 直接 SQL | 结论 |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, r := range rows {
		verdict := "一致"
		if r.metric != r.sql {
			verdict, ok = "**不一致**", false
		}
		fmt.Fprintf(w, "| %s | `%s` | %d | %d | %s |\n", r.item, r.metricsSource, r.metric, r.sql, verdict)
	}
	return ok
}
