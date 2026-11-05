//go:build integration

package repository_test

import (
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2 L8 功能任务 5（隔离、幂等红线）：MarkSelected 只改本农场未进回流的 run（他场同 id 列表不动、重跑 0 行）；
// SelectedRuns 只回本农场 SELECTED；Material 取 ASK 问题 / 时刻、首个 KB_LOAD、有原文且未被取代的 OK 工具调用、田块作物；
// FinishDraft 只从 SELECTED 推进，映射只写 agent_run.backflow_draft_ref，重复推进 → ErrConcurrentUpdate。
func TestBackflowRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repol8bf")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.BackflowRepo{DB: db}
	own := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	other := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}

	l6Exec(t, db, `INSERT INTO field_profile (id, tenant_id, farm_id, ext_field_code, crop, sowing_date, area_mu, prefs, updated_at)
  VALUES (88123, ?, ?, 'bf-it-field', 'rice', '2026-05-01', 12.5, '{}', ?)`, itest.TenantID, itest.FarmID, t0)
	runA := itest.InsertRun(t, db, t0)
	runB := itest.InsertRun(t, db, t0)
	l6Exec(t, db, `UPDATE agent_run SET field_id = 88123 WHERE id = ?`, runA)
	// runB 指向不存在的地块（itest 初始数据里地块 1 是水稻）：测 LEFT JOIN 查不到作物时 Crop 为空。
	l6Exec(t, db, `UPDATE agent_run SET field_id = 99999999 WHERE id = ?`, runB)
	l6Exec(t, db, `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version,
  model_tier, knowledge_ref, created_at, updated_at) VALUES (?, ?, 1, 2001, 'DONE', 'g1', 'p1', 'small', 'kb@test', ?, ?)`,
		itest.TenantID, other.FarmID, t0, t0)
	runOther := uint64(itest.Count(t, db, `SELECT MAX(id) FROM agent_run WHERE farm_id = ?`, other.FarmID))

	at := t0.Add(time.Hour)
	n, err := repo.MarkSelected(ctx, own, []uint64{runA, runB, runOther}, at)
	if err != nil || n != 2 {
		t.Fatalf("MarkSelected = %d, %v; want 2 (other farm untouched)", n, err)
	}
	if n, err := repo.MarkSelected(ctx, own, []uint64{runA, runB}, at); err != nil || n != 0 {
		t.Fatalf("MarkSelected replay = %d, %v; want 0", n, err)
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id = ? AND backflow_status IS NULL`, runOther); c != 1 {
		t.Fatalf("other farm run backflow_status changed")
	}
	if ids, err := repo.SelectedRuns(ctx, other, 10); err != nil || len(ids) != 0 {
		t.Fatalf("other farm SelectedRuns = %v, %v; want none", ids, err)
	}
	ids, err := repo.SelectedRuns(ctx, own, 10)
	if err != nil || len(ids) != 2 || ids[0] != runA || ids[1] != runB {
		t.Fatalf("SelectedRuns = %v, %v; want [%d %d]", ids, err, runA, runB)
	}

	audit := func(farm uint64, action, target, summary string, at time.Time) {
		l6Exec(t, db, `INSERT INTO audit_log (created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, target,
  payload_digest, summary, result) VALUES (?, ?, ?, ?, 'agent', 0, ?, ?, REPEAT('0', 64), ?, 'OK')`,
			at, itest.TenantID, farm, runA, action, target, summary)
	}
	audit(itest.FarmID, "ASK", "field:88123", `{"question":"稻飞虱要打药吗","field_id":88123}`, t0)
	audit(itest.FarmID, "KB_LOAD", "kb:kb-rice@v3", `{}`, t0.Add(time.Second))
	audit(itest.FarmID, "KB_LOAD", "kb:kb-rice@v4", `{}`, t0.Add(2*time.Second))
	audit(other.FarmID, "ASK", "field:1", `{"question":"他场问题"}`, t0.Add(-time.Minute))
	tool := func(farm uint64, key, name, status string, raw any, superseded any) {
		l6Exec(t, db, `INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, tool_name, policy_level,
  args_digest, status, raw_object_id, superseded_at, latency_ms, created_at)
  VALUES (?, ?, ?, 'enrich', LPAD(?, 32, '0'), 1, ?, 'read', REPEAT('0', 64), ?, ?, ?, 10, ?)`,
			itest.TenantID, farm, runA, key, name, status, raw, superseded, t0)
	}
	tool(itest.FarmID, "a", "weather.forecast", model.ToolStatusOK, 501, nil)
	tool(itest.FarmID, "b", "field.profile", model.ToolStatusOK, 502, nil)
	tool(itest.FarmID, "c", "weather.forecast", "TIMEOUT", 503, nil)          // 非 OK
	tool(itest.FarmID, "d", "pest.risk", model.ToolStatusOK, nil, nil)        // 无原文
	tool(itest.FarmID, "e", "pest.risk", model.ToolStatusOK, 504, t0)         // 已被取代
	tool(other.FarmID, "f", "weather.forecast", model.ToolStatusOK, 505, nil) // 他场

	m, err := repo.Material(ctx, own, runA)
	if err != nil {
		t.Fatal(err)
	}
	if m.RunID != runA || m.FieldID != 88123 || m.UserID != 1001 || m.Crop != "rice" || m.Question != "稻飞虱要打药吗" ||
		!m.AskedAt.Equal(t0) || m.KnowledgeRef != "kb-rice@v3" {
		t.Fatalf("material = %+v", m)
	}
	if len(m.Tools) != 2 || m.Tools[0] != (model.BackflowTool{Name: "weather.forecast", RawID: 501}) ||
		m.Tools[1] != (model.BackflowTool{Name: "field.profile", RawID: 502}) {
		t.Fatalf("material tools = %+v", m.Tools)
	}
	if mb, err := repo.Material(ctx, own, runB); err != nil || mb.KnowledgeRef != "kb@test" || mb.Crop != "" || len(mb.Tools) != 0 {
		t.Fatalf("material without audit = %+v, %v", mb, err)
	}
	if _, err := repo.Material(ctx, other, runA); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other farm Material err = %v, want ErrNotFound", err)
	}

	if err := repo.FinishDraft(ctx, other, runA, model.BackflowDrafted, "bf-abcdefghijklmnop", at); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("other farm FinishDraft err = %v, want ErrConcurrentUpdate", err)
	}
	if err := repo.FinishDraft(ctx, own, runA, model.BackflowDrafted, "bf-abcdefghijklmnop", at); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDraft(ctx, own, runB, model.BackflowPIIFailed, "", at); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDraft(ctx, own, runA, model.BackflowPIIFailed, "", at); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("replay FinishDraft err = %v, want ErrConcurrentUpdate", err)
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id = ? AND backflow_status = 'DRAFTED'
  AND backflow_draft_ref = 'bf-abcdefghijklmnop'`, runA); c != 1 {
		t.Fatal("runA not DRAFTED with draft_ref")
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id = ? AND backflow_status = 'PII_FAILED'
  AND backflow_draft_ref IS NULL`, runB); c != 1 {
		t.Fatal("runB not PII_FAILED without draft_ref")
	}

	// 状态事件：每次推进一条 BACKFLOW 审计（SELECTED 两条 + DRAFTED + PII_FAILED），summary 不含 draft_ref。
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND farm_id = ? AND action = 'BACKFLOW'`,
		itest.TenantID, itest.FarmID); c != 4 {
		t.Fatalf("BACKFLOW events = %d, want 4", c)
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM audit_log WHERE action = 'BACKFLOW' AND summary LIKE '%bf-%'`); c != 0 {
		t.Fatal("BACKFLOW event leaks draft_ref")
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM audit_log WHERE farm_id = ? AND action = 'BACKFLOW'`, other.FarmID); c != 0 {
		t.Fatal("other farm got BACKFLOW events")
	}

	// 失败尝试（卡死修复）：前两次保持 SELECTED，第 BackflowMaxAttempts 次置 FAILED，不再进 SelectedRuns；
	// 他场 Scope 与非 SELECTED 的 run → ErrConcurrentUpdate。
	runC := itest.InsertRun(t, db, t0)
	if _, err := repo.MarkSelected(ctx, own, []uint64{runC}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FailAttempt(ctx, other, runC, model.BackflowMaxAttempts, at); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("other farm FailAttempt err = %v, want ErrConcurrentUpdate", err)
	}
	for i := 1; i <= model.BackflowMaxAttempts; i++ {
		failed, err := repo.FailAttempt(ctx, own, runC, model.BackflowMaxAttempts, at)
		if err != nil || failed != (i == model.BackflowMaxAttempts) {
			t.Fatalf("FailAttempt #%d = %v, %v", i, failed, err)
		}
	}
	if ids, err := repo.SelectedRuns(ctx, own, 10); err != nil || len(ids) != 0 {
		t.Fatalf("SelectedRuns after FAILED = %v, %v; want none", ids, err)
	}
	if c := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id = ? AND backflow_status = 'FAILED' AND backflow_draft_ref IS NULL`,
		runC); c != 1 {
		t.Fatal("runC not FAILED")
	}
	if _, err := repo.FailAttempt(ctx, own, runC, model.BackflowMaxAttempts, at); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("FailAttempt on FAILED err = %v, want ErrConcurrentUpdate", err)
	}

	// 周报回流计数按事件时刻（不受 agent_run.updated_at 的其他写入影响），每状态按 run 去重，RETRY 不计。
	l6Exec(t, db, `UPDATE agent_run SET updated_at = ? WHERE id IN (?, ?, ?)`, t0.AddDate(0, 0, 30), runA, runB, runC)
	counts, err := repository.AttributionRepo{DB: db}.BackflowCounts(ctx, own, t0, t0.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if counts[model.BackflowSelected] != 3 || counts[model.BackflowDrafted] != 1 || counts[model.BackflowPIIFailed] != 1 ||
		counts[model.BackflowFailed] != 1 || counts[model.BackflowRetry] != 0 {
		t.Fatalf("BackflowCounts = %v", counts)
	}
	if later, err := (repository.AttributionRepo{DB: db}).BackflowCounts(ctx, own, t0.AddDate(0, 0, 7), t0.AddDate(0, 0, 14)); err != nil ||
		len(later) != 0 {
		t.Fatalf("next week BackflowCounts = %v, %v; want none", later, err)
	}
}
