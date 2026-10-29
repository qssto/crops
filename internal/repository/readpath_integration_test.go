//go:build integration

package repository_test

import (
	"fmt"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// l7Event 插一条 run_event（seq 显式给定；toolCallID 为 0 时写 NULL）。
func l7Event(t *testing.T, db *repository.DB, runID uint64, seq int, step string, toolCallID uint64, typ model.EventType,
	payload string, at time.Time) {
	t.Helper()
	var tc any
	if toolCallID != 0 {
		tc = toolCallID
	}
	var st any
	if step != "" {
		st = step
	}
	l6Exec(t, db, `INSERT INTO run_event (tenant_id, farm_id, run_id, seq, step_id, tool_call_id, event_type, actor, payload, created_at)
  VALUES (?, ?, ?, ?, ?, ?, ?, 'agent', ?, ?)`, itest.TenantID, itest.FarmID, runID, seq, st, tc, string(typ), payload, at)
}

// l7Tool 插一条 tool_call，返回 id。
func l7Tool(t *testing.T, db *repository.DB, runID uint64, key, status string, latency int, at time.Time) uint64 {
	t.Helper()
	l6Exec(t, db, `INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, tool_name, policy_level,
  args_digest, status, latency_ms, created_at) VALUES (?, ?, ?, 'enrich', ?, 1, 'weather.forecast', 'read', ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, runID, fmt.Sprintf("%032s", key), fmt.Sprintf("%064d", 0), status, latency, at)
	return uint64(itest.Count(t, db, `SELECT MAX(id) FROM tool_call WHERE run_id=?`, runID))
}

// 验收 1 / 4：RunDetailRepo 每表一次查询读回完整链路；MetricsRepo 的事实行与直接 COUNT 对账一致。
func TestReadPath_Integration(t *testing.T) {
	e := itest.Setup(t, "repol7")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	runID := itest.InsertRun(t, db, t0)
	l6Exec(t, db, `INSERT INTO run_step (tenant_id, farm_id, run_id, step_id, node, status, started_at)
  VALUES (?, ?, ?, 'enrich', 'enrich', 'COMPLETED', ?)`, itest.TenantID, itest.FarmID, runID, t0)
	okID := l7Tool(t, db, runID, "a", model.ToolStatusOK, 120, t0)
	l7Tool(t, db, runID, "b", "TIMEOUT", 3000, t0)
	l7Event(t, db, runID, 1, "", 0, model.EventRunStarted, `{"question":"q","field_id":1}`, t0)
	l7Event(t, db, runID, 2, "enrich", okID, model.EventToolCallEnd, `{"tool":"weather.forecast","status":"OK"}`, t0)
	l7Event(t, db, runID, 3, "advise", 0, model.EventLLMCallEnd, `{"task":"advise","status":"OK","prompt_tokens":10,"completion_tokens":2}`, t0)
	l7Event(t, db, runID, 4, "validate", 0, model.EventValidationFailed, `{}`, t0)
	orderID := itest.InsertOrder(t, db, runID, string(model.OrderDraft), t0)
	l6Exec(t, db, `INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status, actor_type, actor_id, created_at)
  VALUES (?, ?, ?, ?, 'DRAFT', 'PENDING_APPROVAL', 'system', 0, ?)`, itest.TenantID, itest.FarmID, orderID, runID, t0)
	l6Exec(t, db, `INSERT INTO audit_log (created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, payload_digest, result, raw_object_id)
  VALUES (?, ?, ?, ?, 'agent', 0, ?, ?, 'OK', 91)`, t0, itest.TenantID, itest.FarmID, runID, string(model.AuditLLMCall),
		fmt.Sprintf("%064d", 0))

	runs := repository.NewRunRepo(db, slot.Clock)
	rows, err := repository.NewRunDetailRepo(db, runs, repository.NewAdviceRepo(db, slot.Clock)).Load(ctx, l4Scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Card != nil || len(rows.Steps) != 1 || len(rows.Events) != 4 || len(rows.ToolCalls) != 2 ||
		len(rows.Orders) != 1 || len(rows.Transitions) != 1 || len(rows.Audits) != 1 || len(rows.Tickets) != 0 {
		t.Fatalf("rows = card %v steps %d events %d tools %d orders %d transitions %d audits %d tickets %d",
			rows.Card, len(rows.Steps), len(rows.Events), len(rows.ToolCalls), len(rows.Orders), len(rows.Transitions),
			len(rows.Audits), len(rows.Tickets))
	}
	if ev := rows.Events[1]; ev.ToolCallID == nil || *ev.ToolCallID != okID || ev.StepID != "enrich" {
		t.Errorf("event 2 = %+v", ev)
	}
	if _, err := repository.NewRunDetailRepo(db, runs, repository.NewAdviceRepo(db, slot.Clock)).
		Load(ctx, model.Scope{TenantID: itest.TenantID, FarmID: 2}, runID); err == nil {
		t.Error("other farm can read run detail")
	}

	from, to := t0.Truncate(24*time.Hour), t0.Truncate(24*time.Hour).Add(24*time.Hour)
	f, err := repository.NewMetricsRepo(db).Facts(ctx, l4Scope, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// 对账：事实行条数 = 直接 COUNT
	checks := []struct {
		name string
		got  int
		q    string
	}{
		{"runs", len(f.Runs), `SELECT COUNT(*) FROM agent_run WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`},
		{"tool_calls", len(f.ToolCalls), `SELECT COUNT(*) FROM tool_call WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`},
		{"orders", len(f.Orders), `SELECT COUNT(*) FROM work_order WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<?`},
	}
	for _, c := range checks {
		if want := itest.Count(t, db, c.q, itest.TenantID, itest.FarmID, from, to); int64(c.got) != want {
			t.Errorf("%s: facts %d, COUNT %d", c.name, c.got, want)
		}
	}
	// 事件只取指标类型：LLM_CALL_END + VALIDATION_FAILED
	if len(f.Events) != 2 || len(f.Orders) != 1 || f.Orders[0].DispatchMode != string(model.DispatchDryRun) || f.Orders[0].Dispatched {
		t.Errorf("events %d orders %+v", len(f.Events), f.Orders)
	}
}
