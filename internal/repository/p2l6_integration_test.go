//go:build integration

package repository_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2 L6 长对话压缩的 agent_run 读写（代码 + 注入时钟）。

var p2l6Meta = agent.RunMeta{GraphVersion: "g1", PromptVersion: "p1", ModelTier: "small", KnowledgeRef: "kb@test"}

var p2l6State = `{"compaction_version":1,"facts":{"field_id":1},"summarized_until_step_id":"run-1"}`

// p2l6Run 插一个指定状态 / 用户 / 地块的 run，可带 compaction_state 与 prev_run_id（0 为 NULL），updated_at 取 at。
func p2l6Run(t *testing.T, db *repository.DB, status string, userID, fieldID uint64, state string, prev uint64,
	at time.Time) uint64 {
	t.Helper()
	var st, pr any
	if state != "" {
		st = state
	}
	if prev != 0 {
		pr = prev
	}
	l6Exec(t, db, `INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version,
  model_tier, knowledge_ref, compaction_state, prev_run_id, created_at, updated_at)
  VALUES (?, ?, ?, ?, ?, 'g1', 'p1', 'small', 'kb@test', ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, fieldID, userID, status, st, pr, at, at)
	return uint64(itest.Count(t, db, `SELECT MAX(id) FROM agent_run`))
}

// p2l6Link 读新 run 的 prev_run_id（NULL 为 0）与 compaction_state（NULL 为空串）。
func p2l6Link(t *testing.T, db *repository.DB, runID uint64) (uint64, string) {
	t.Helper()
	var row struct {
		PrevRunID       *uint64
		CompactionState *string
	}
	if err := db.WithContext(t.Context()).Raw(`SELECT prev_run_id, compaction_state FROM agent_run WHERE id = ?`, runID).
		Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	var prev uint64
	var state string
	if row.PrevRunID != nil {
		prev = *row.PrevRunID
	}
	if row.CompactionState != nil {
		state = *row.CompactionState
	}
	return prev, state
}

// p2l6SameJSON 比较两段 JSON 语义相同（MySQL json 列会重排键与空白）。
func p2l6SameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("decode %q: %v", want, err)
	}
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ja) == string(jb)
}

// 会话续接（技术实现 2）：同租户 / 农场 / 地块 / 用户、终态、updated_at ≥ now - idle 才续接，
// 续接时同一事务写 prev_run_id 并复制 compaction_state；idle 边界用注入时钟验证（恰好 idle 仍续接，超 1ms 开新会话）。
func TestCreateRunLinked_Session_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.NewRunRepo(db, slot.Clock)
	rs := model.RunState{TenantID: itest.TenantID, FarmID: itest.FarmID}
	idle := 30 * time.Minute
	prev := p2l6Run(t, db, "COMPLETED", 1001, 1, p2l6State, 0, t0)
	link := model.RunLink{Kind: model.RunLinkSession, RunID: prev, Idle: idle}

	slot.Clock.Set(t0.Add(idle))
	id, linked, err := repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta, link)
	if err != nil || !linked {
		t.Fatalf("at idle boundary: linked=%v err=%v, want linked", linked, err)
	}
	if p, st := p2l6Link(t, db, id); p != prev || !p2l6SameJSON(t, st, p2l6State) {
		t.Fatalf("linked run prev=%d state=%s, want prev=%d + copied state", p, st, prev)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='RUNNING' AND compaction_count=0`, id); n != 1 {
		t.Fatal("linked run not RUNNING with compaction_count 0")
	}

	for name, tc := range map[string]struct {
		at          time.Time
		user, field uint64
		link        model.RunLink
	}{
		"idle exceeded by 1ms": {at: t0.Add(idle + time.Millisecond), user: 1001, field: 1, link: link},
		"other user":           {at: t0, user: 1002, field: 1, link: link},
		"other field":          {at: t0, user: 1001, field: 2, link: link},
		"no link":              {at: t0, user: 1001, field: 1, link: model.RunLink{}},
	} {
		t.Run(name, func(t *testing.T) {
			slot.Clock.Set(tc.at)
			id, linked, err := repo.CreateRunLinked(ctx, rs, tc.user, tc.field, p2l6Meta, tc.link)
			if err != nil || linked {
				t.Fatalf("linked=%v err=%v, want new session", linked, err)
			}
			if p, st := p2l6Link(t, db, id); p != 0 || st != "" {
				t.Fatalf("new session prev=%d state=%q, want none", p, st)
			}
		})
	}

	running := p2l6Run(t, db, "RUNNING", 1001, 1, p2l6State, 0, t0)
	slot.Clock.Set(t0)
	if _, linked, err := repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta,
		model.RunLink{Kind: model.RunLinkSession, RunID: running, Idle: idle}); err != nil || linked {
		t.Fatalf("non-terminal prev: linked=%v err=%v, want new session", linked, err)
	}
	// 上一 run 无压缩状态也续接（只写 prev_run_id），会话链由此延续
	bare := p2l6Run(t, db, "FAILED", 1001, 1, "", 0, t0)
	id, linked, err = repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta,
		model.RunLink{Kind: model.RunLinkSession, RunID: bare, Idle: idle})
	if err != nil || !linked {
		t.Fatalf("bare prev: linked=%v err=%v", linked, err)
	}
	if p, st := p2l6Link(t, db, id); p != bare || st != "" {
		t.Fatalf("bare prev link prev=%d state=%q", p, st)
	}
	if _, _, err := repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta,
		model.RunLink{Kind: model.RunLinkSession, RunID: bare}); err == nil {
		t.Fatal("session link with zero idle accepted")
	}
}

// 复检取代（技术实现 3）：新 run 接替旧 run 在会话链中的位置（prev_run_id 取旧 run 的），并复制旧 run 的压缩状态；
// 旧 run 无链无状态、不同地块时开新会话。
func TestCreateRunLinked_Supersede_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.NewRunRepo(db, slot.Clock)
	rs := model.RunState{TenantID: itest.TenantID, FarmID: itest.FarmID}
	first := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, t0)
	old := p2l6Run(t, db, "INTERRUPTED", 1001, 1, p2l6State, first, t0)

	id, linked, err := repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta, model.RunLink{Kind: model.RunLinkSupersede, RunID: old})
	if err != nil || !linked {
		t.Fatalf("supersede: linked=%v err=%v", linked, err)
	}
	if p, st := p2l6Link(t, db, id); p != first || !p2l6SameJSON(t, st, p2l6State) {
		t.Fatalf("supersede run prev=%d state=%s, want prev=%d + copied state", p, st, first)
	}
	for name, tc := range map[string]struct {
		runID, field uint64
	}{
		"no chain no state": {runID: first, field: 1},
		"other field":       {runID: old, field: 2},
	} {
		t.Run(name, func(t *testing.T) {
			id, linked, err := repo.CreateRunLinked(ctx, rs, 1001, tc.field, p2l6Meta,
				model.RunLink{Kind: model.RunLinkSupersede, RunID: tc.runID})
			if err != nil || linked {
				t.Fatalf("linked=%v err=%v, want new session", linked, err)
			}
			if p, st := p2l6Link(t, db, id); p != 0 || st != "" {
				t.Fatalf("prev=%d state=%q, want none", p, st)
			}
		})
	}
	if _, _, err := repo.CreateRunLinked(ctx, rs, 1001, 1, p2l6Meta, model.RunLink{Kind: "BOGUS", RunID: old}); err == nil {
		t.Fatal("unknown link kind accepted")
	}
}

// 技术实现 6：载体带压缩状态时，中断事务与终态 UPDATE 一并写 compaction_state / compaction_count（row_ver 守卫），
// interrupt_type / reason_code 同事务补写；无载体（开关关）时两列保持 P1 的 NULL / 0。
func TestCompactionStatePersisted_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db := slot.DB
	runs := repository.NewRunRepo(db, slot.Clock)
	ckpt := repository.NewCheckpointRepo(db, slot.Clock, nil, nil, "")
	st := &model.CompactionState{CompactionVersion: model.CompactionVersion, Facts: model.CompactionFacts{FieldID: 1},
		SummarizedUntilStepID: "run-7", RawRefs: []uint64{11}}
	want, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	carried := func(runID uint64) (agent.CheckpointTarget, func() (string, int64)) {
		target := agent.CheckpointTarget{Scope: l4Scope, RunID: runID, Interrupt: model.InterruptWaitApproval}
		read := func() (string, int64) {
			var row struct {
				CompactionState *string
				CompactionCount int64
			}
			if err := db.WithContext(t.Context()).Raw(`SELECT compaction_state, compaction_count FROM agent_run WHERE id=?`,
				runID).Scan(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.CompactionState == nil {
				return "", row.CompactionCount
			}
			return *row.CompactionState, row.CompactionCount
		}
		return target, read
	}

	// 中断 → 恢复 → 终态，全程带载体
	runID := itest.InsertRun(t, db, t0)
	target, read := carried(runID)
	ctx, carrier := agent.WithCompactionCarrier(agent.WithCheckpointTarget(t.Context(), target))
	carrier.Set(st, 1)
	if err := ckpt.Set(ctx, strconv.FormatUint(runID, 10), []byte("ckpt")); err != nil {
		t.Fatal(err)
	}
	if s, n := read(); !p2l6SameJSON(t, s, string(want)) || n != 1 {
		t.Fatalf("after interrupt state=%s count=%d", s, n)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='INTERRUPTED'
  AND interrupt_type='WAIT_APPROVAL' AND row_ver=1`, runID); n != 1 {
		t.Fatal("interrupt with compaction: status / interrupt_type / row_ver wrong")
	}
	l6Exec(t, db, `UPDATE agent_run SET status='RUNNING' WHERE id=?`, runID)
	carrier.Set(st, 2)
	if err := runs.FinishRun(ctx, l4Scope, runID, model.RunFailed, "LLM_DOWN"); err != nil {
		t.Fatal(err)
	}
	if s, n := read(); !p2l6SameJSON(t, s, string(want)) || n != 2 {
		t.Fatalf("after finish state=%s count=%d", s, n)
	}
	if n := itest.Count(t, db, `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='FAILED' AND reason_code='LLM_DOWN'
  AND row_ver=2`, runID); n != 1 {
		t.Fatal("finish with compaction: status / reason_code / row_ver wrong")
	}

	// 无载体 / 空载体：与 P1 相同
	for name, withCarrier := range map[string]bool{"no carrier": false, "empty carrier": true} {
		t.Run(name, func(t *testing.T) {
			runID := itest.InsertRun(t, db, t0)
			target, read := carried(runID)
			ctx := agent.WithCheckpointTarget(t.Context(), target)
			if withCarrier {
				ctx, _ = agent.WithCompactionCarrier(ctx)
			}
			if err := ckpt.Set(ctx, strconv.FormatUint(runID, 10), []byte("ckpt")); err != nil {
				t.Fatal(err)
			}
			l6Exec(t, db, `UPDATE agent_run SET status='RUNNING' WHERE id=?`, runID)
			if err := runs.FinishRun(ctx, l4Scope, runID, model.RunCompleted, ""); err != nil {
				t.Fatal(err)
			}
			if s, n := read(); s != "" || n != 0 {
				t.Fatalf("state=%q count=%d, want NULL / 0", s, n)
			}
		})
	}

	// 带载体时已终态的 run 再 Finish 仍是非法迁移（状态检查在 compaction UPDATE 之前）
	if err := runs.FinishRun(ctx, l4Scope, runID, model.RunCompleted, ""); !errors.Is(err, model.ErrIllegalTransition) {
		t.Fatalf("second finish: %v, want illegal transition", err)
	}
}

// p2l6Card 插一张建议卡（numbers / selected_product 可空），返回 id。
func p2l6Card(t *testing.T, db *repository.DB, runID uint64, outcome, conclusion, numbers, product string, at time.Time) uint64 {
	t.Helper()
	var nums, prod any
	if numbers != "" {
		nums = numbers
	}
	if product != "" {
		prod = product
	}
	l6Exec(t, db, `INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, next_action, numbers, selected_product, schema_ver, created_at)
  VALUES (?, ?, 1, ?, 'rice', 'tillering', 'pest', 'spray', ?, ?, '[]', 'NONE', ?, ?, 1, ?)`,
		itest.TenantID, itest.FarmID, runID, outcome, conclusion, nums, prod, at)
	return uint64(itest.Count(t, db, `SELECT MAX(id) FROM advice_card WHERE run_id=?`, runID))
}

// 技术实现 3 / 4：LoadSession 沿 prev_run_id 由早到晚取会话链，轮次取各 run 的 RUN_STARTED 问题与卡结论，
// 作业单取最新 ORDER 票与驳回原因，实测只取未被取代的 time_sensitive，引用只取 type=kb；跨农场 run 不在链上。
func TestLoadSession_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.NewRunRepo(db, slot.Clock)

	r1 := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, t0)
	r2 := p2l6Run(t, db, "COMPLETED", 1001, 1, "", r1, t0.Add(time.Minute))
	r3 := p2l6Run(t, db, "RUNNING", 1001, 1, p2l6State, r2, t0.Add(2*time.Minute))
	other := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, t0) // 不在链上
	for i, id := range []uint64{r1, r2, r3, other} {
		l7Event(t, db, id, 1, "", 0, model.EventRunStarted,
			fmt.Sprintf(`{"field_id":1,"question":"问题%d","channel":"web"}`, i+1), t0)
	}
	c1 := p2l6Card(t, db, r1, "ADVICE", "喷吡蚜酮", `[{"field":"dose","value":20,"unit":"g/亩"}]`, "吡蚜酮", t0)
	p2l6Card(t, db, other, "ADVICE", "不该出现", "", "", t0)
	// r1 的作业单：两张票，取 ticket_seq 最大且已驳回的一张
	l6Exec(t, db, `INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload,
  time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (?, ?, 1, ?, ?, 'SPRAY', 'DRONE', '{}', ?, ?, 'REJECTED', 'DRY_RUN', 1, ?, ?)`,
		itest.TenantID, itest.FarmID, c1, r1, t0, t0.Add(time.Hour), t0, t0)
	orderID := uint64(itest.Count(t, db, `SELECT id FROM work_order WHERE advice_id=?`, c1))
	itest.InsertTicket(t, db, r1, orderID, "SUPERSEDED", t0)
	l6Exec(t, db, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_kind, ticket_seq,
  risk_level, required_role, initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 'ORDER', 2, 'LOW', '["agronomist"]', 1001, ?, 1, 'REJECTED', ?, ?, ?)`,
		itest.TenantID, itest.FarmID, r1, orderID, fmt.Sprintf("%064d", 0), t0.Add(time.Hour), t0, t0)
	ticket2 := uint64(itest.Count(t, db, `SELECT id FROM approval_ticket WHERE order_id=? AND ticket_seq=2`, orderID))
	l6Exec(t, db, `INSERT INTO rejection_reason (tenant_id, farm_id, ticket_id, order_id, advice_id, run_id, reason_code,
  reason_text, operator_id, operator_role, created_at) VALUES (?, ?, ?, ?, ?, ?, 'PRODUCT', '换药', 2001, 'agronomist', ?)`,
		itest.TenantID, itest.FarmID, ticket2, orderID, c1, r1, t0)
	l6Exec(t, db, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, 0, 'conclusion', 'kb', 'kb-7', 'v1')`, itest.TenantID, itest.FarmID, c1)
	l6Exec(t, db, `INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, 1, 'numbers[0]', 'measured', NULL, NULL)`, itest.TenantID, itest.FarmID, c1)
	// r2 的作业单被用户中止：迁移记录取进入 CANCELED 的一条（CANCELED_BY_USER）
	c2 := p2l6Card(t, db, r2, "ADVICE", "喷噻虫嗪", "", "噻虫嗪", t0)
	l6Exec(t, db, `INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload,
  time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (?, ?, 1, ?, ?, 'SPRAY', 'DRONE', '{}', ?, ?, 'CANCELED', 'DRY_RUN', 1, ?, ?)`,
		itest.TenantID, itest.FarmID, c2, r2, t0, t0.Add(time.Hour), t0, t0)
	canceled := uint64(itest.Count(t, db, `SELECT id FROM work_order WHERE advice_id=?`, c2))
	for _, tr := range [][2]string{{"PENDING_APPROVAL", "APPROVED"}, {"APPROVED", "CANCELED"}} {
		reason := map[string]string{"APPROVED": "APPROVED", "CANCELED": "CANCELED_BY_USER"}[tr[1]]
		l6Exec(t, db, `INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status,
  actor_type, actor_id, reason_code, created_at) VALUES (?, ?, ?, ?, ?, ?, 'user', 1001, ?, ?)`,
			itest.TenantID, itest.FarmID, canceled, r2, tr[0], tr[1], reason, t0)
	}
	// r2 两条实测：一条有效、一条已被取代；r3 一条非时效
	tc := l7Tool(t, db, r2, "a", "OK", 10, t0)
	l6Exec(t, db, `UPDATE tool_call SET time_sensitive=1, result_summary='{"rain_mm":3}' WHERE id=?`, tc)
	gone := l7Tool(t, db, r2, "b", "OK", 10, t0)
	l6Exec(t, db, `UPDATE tool_call SET time_sensitive=1, superseded_at=? WHERE id=?`, t0, gone)
	l7Tool(t, db, r3, "c", "OK", 10, t0)

	src, err := repo.LoadSession(ctx, l4Scope, r3)
	if err != nil {
		t.Fatal(err)
	}
	if src.RunID != r3 || src.FieldID != 1 || !p2l6SameJSON(t, string(src.State), p2l6State) {
		t.Fatalf("head = %d/%d state=%s", src.RunID, src.FieldID, src.State)
	}
	if len(src.Turns) != 2 || src.Turns[0].RunID != r1 || src.Turns[0].Question != "问题1" ||
		src.Turns[0].Answer != "喷吡蚜酮" || src.Turns[1].RunID != r2 || src.Turns[1].Answer != "喷噻虫嗪" {
		t.Fatalf("turns = %+v, want r1, r2 (both with answers)", src.Turns)
	}
	if len(src.Cards) != 2 || src.Cards[0].AdviceID != c1 || src.Cards[1].AdviceID != c2 || src.Cards[0].SelectedProduct != "吡蚜酮" ||
		len(src.Cards[0].Numbers) != 1 || src.Cards[0].Numbers[0].Value != 20 || src.Cards[0].Window == nil ||
		!src.Cards[0].Window.From.Equal(t0) {
		t.Fatalf("cards = %+v", src.Cards)
	}
	if len(src.Orders) != 2 || src.Orders[0].OrderID != orderID || src.Orders[0].AdviceID != c1 ||
		src.Orders[0].TicketID != ticket2 ||
		src.Orders[0].TicketStatus != model.TicketRejected || src.Orders[0].RejectReasonCode != "PRODUCT" ||
		src.Orders[0].RejectText != "换药" || src.Orders[0].Product != "吡蚜酮" || src.Orders[0].RunID != r1 ||
		src.Orders[0].CancelReason != "" {
		t.Fatalf("orders[0] = %+v", src.Orders[0])
	}
	if o := src.Orders[1]; o.OrderID != canceled || o.AdviceID != c2 || o.Status != model.OrderCanceled ||
		o.CancelReason != model.ReasonCanceledByUser || o.Product != "噻虫嗪" || o.TicketID != 0 {
		t.Fatalf("orders[1] = %+v, want user-canceled order of c2", o)
	}
	if len(src.Measured) != 1 || src.Measured[0].ToolCallID != tc || !p2l6SameJSON(t, string(src.Measured[0].ResultSummary), `{"rain_mm":3}`) ||
		!src.Measured[0].CollectedAt.Equal(t0) {
		t.Fatalf("measured = %+v", src.Measured)
	}
	if len(src.CitedKBIDs) != 1 || src.CitedKBIDs[0] != "kb-7" {
		t.Fatalf("kb ids = %v", src.CitedKBIDs)
	}

	// 首轮：链上只有本 run，没有轮次
	first, err := repo.LoadSession(ctx, l4Scope, r1)
	if err != nil || first.Turns != nil || len(first.Cards) != 1 || first.State != nil {
		t.Fatalf("first turn src = %+v err=%v", first, err)
	}
	if _, err := repo.LoadSession(ctx, model.Scope{TenantID: 1, FarmID: 99, UserID: 1001}, r3); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other farm: %v, want not found", err)
	}
}

// 会话链超过回溯上限（32）：链只取最近 32 个 run 并标 Truncated；被截掉的同地块同用户更早 run 的问题与作业单
// 按地块汇总进 EarlierTurns / EarlierOrders（不依赖链深），别的用户 / 地块的 run 不混入。
func TestLoadSession_TruncatedChain_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.NewRunRepo(db, slot.Clock)

	// 别的用户 / 地块的 run 先插（id 早于链首），不得混入汇总
	strangerOther := p2l6Run(t, db, "COMPLETED", 1002, 1, "", 0, t0)
	fieldOther := p2l6Run(t, db, "COMPLETED", 1001, 2, "", 0, t0)
	for _, id := range []uint64{strangerOther, fieldOther} {
		l7Event(t, db, id, 1, "", 0, model.EventRunStarted, `{"field_id":1,"question":"不能用敌敌畏","channel":"web"}`, t0)
	}
	const n = 35
	runs := make([]uint64, n)
	for i := range n {
		var prev uint64
		if i > 0 {
			prev = runs[i-1]
		}
		runs[i] = p2l6Run(t, db, "COMPLETED", 1001, 1, "", prev, t0.Add(time.Duration(i)*time.Minute))
		q := fmt.Sprintf("问题%d", i+1)
		if i == 0 {
			q = "这块地不能用毒死蜱"
		}
		l7Event(t, db, runs[i], 1, "", 0, model.EventRunStarted,
			fmt.Sprintf(`{"field_id":1,"question":%q,"channel":"web"}`, q), t0)
	}
	head := runs[n-1]
	c := p2l6Card(t, db, runs[0], "ADVICE", "喷毒死蜱", "", "毒死蜱", t0)
	l6Exec(t, db, `INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload,
  time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (?, ?, 1, ?, ?, 'SPRAY', 'DRONE', '{}', ?, ?, 'REJECTED', 'DRY_RUN', 1, ?, ?)`,
		itest.TenantID, itest.FarmID, c, runs[0], t0, t0.Add(time.Hour), t0, t0)
	orderID := uint64(itest.Count(t, db, `SELECT id FROM work_order WHERE advice_id=?`, c))
	ticket := itest.InsertTicket(t, db, runs[0], orderID, "REJECTED", t0)
	l6Exec(t, db, `INSERT INTO rejection_reason (tenant_id, farm_id, ticket_id, order_id, advice_id, run_id, reason_code,
  reason_text, operator_id, operator_role, created_at) VALUES (?, ?, ?, ?, ?, ?, 'PRODUCT', '禁用药', 2001, 'agronomist', ?)`,
		itest.TenantID, itest.FarmID, ticket, orderID, c, runs[0], t0)

	src, err := repo.LoadSession(ctx, l4Scope, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Turns) != 31 || src.Turns[0].RunID != runs[n-32] {
		t.Fatalf("chain turns = %d (first %d), want 31 from run %d", len(src.Turns), src.Turns[0].RunID, runs[n-32])
	}
	if !src.Truncated || src.EarlierRuns != n-32 || len(src.EarlierTurns) != n-32 {
		t.Fatalf("truncated=%v earlier runs=%d turns=%d, want %d", src.Truncated, src.EarlierRuns, len(src.EarlierTurns), n-32)
	}
	if et := src.EarlierTurns[0]; et.RunID != runs[0] || et.Question != "这块地不能用毒死蜱" {
		t.Fatalf("earlier turn[0] = %+v", et)
	}
	if len(src.EarlierOrders) != 1 || src.EarlierOrders[0].OrderID != orderID || src.EarlierOrders[0].TicketID != ticket ||
		src.EarlierOrders[0].RejectReasonCode != "PRODUCT" || src.EarlierOrders[0].Product != "毒死蜱" {
		t.Fatalf("earlier orders = %+v", src.EarlierOrders)
	}
	if len(src.Orders) != 0 || len(src.Cards) != 0 {
		t.Fatalf("chain orders=%d cards=%d, want earlier rows kept out of chain", len(src.Orders), len(src.Cards))
	}

	// 恰好 32 层（链首无 prev）不算截断
	exact, err := repo.LoadSession(ctx, l4Scope, runs[31])
	if err != nil || exact.Truncated || exact.EarlierTurns != nil || len(exact.Turns) != 31 {
		t.Fatalf("exact 32 chain: truncated=%v earlier=%d turns=%d err=%v", exact.Truncated, len(exact.EarlierTurns),
			len(exact.Turns), err)
	}
}

// 压缩触发率（日报 SQL）：窗口内终态 run 中 compaction_count > 0 的比例；RUNNING 与窗口外不计；无 run 为 0。
// CompactedRawIDs 只列本 run 的 COMPACTED_MSGS。
func TestCompactionRateAndRawIDs_Integration(t *testing.T) {
	e := itest.Setup(t, "repop2l6")
	slot := e.Fresh(t, t0)
	db, ctx := slot.DB, t.Context()
	repo := repository.NewRunRepo(db, slot.Clock)
	from, to := t0, t0.Add(24*time.Hour)
	if rate, err := repo.CompactionRate(ctx, l4Scope, from, to); err != nil || rate != 0 {
		t.Fatalf("empty rate = %v err=%v", rate, err)
	}
	a := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, t0.Add(time.Hour))
	p2l6Run(t, db, "FAILED", 1001, 1, "", 0, t0.Add(time.Hour))
	p2l6Run(t, db, "CANCELED", 1001, 1, "", 0, t0.Add(time.Hour))
	b := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, t0.Add(time.Hour))
	running := p2l6Run(t, db, "RUNNING", 1001, 1, "", 0, t0.Add(time.Hour))
	late := p2l6Run(t, db, "COMPLETED", 1001, 1, "", 0, to)
	l6Exec(t, db, `UPDATE agent_run SET compaction_count=1 WHERE id IN (?, ?, ?, ?)`, a, b, running, late)
	l6Exec(t, db, `UPDATE agent_run SET compaction_count=3 WHERE id=?`, b)
	rate, err := repo.CompactionRate(ctx, l4Scope, from, to)
	if err != nil || rate != 0.5 {
		t.Fatalf("rate = %v err=%v, want 0.5", rate, err)
	}

	for i, kind := range []string{"COMPACTED_MSGS", "CHECKPOINT", "COMPACTED_MSGS"} {
		l6Exec(t, db, `INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key, sha256, size_bytes, created_at)
  VALUES (?, ?, ?, ?, ?, ?, 1, ?)`, itest.TenantID, itest.FarmID, a, kind, fmt.Sprintf("p2l6/%d/%d", a, i),
			fmt.Sprintf("%064d", i), t0)
	}
	l6Exec(t, db, `INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key, sha256, size_bytes, created_at)
  VALUES (?, ?, ?, 'COMPACTED_MSGS', ?, ?, 1, ?)`, itest.TenantID, itest.FarmID, b, fmt.Sprintf("p2l6/%d/x", b),
		fmt.Sprintf("%064d", 9), t0)
	ids, err := repo.CompactedRawIDs(ctx, l4Scope, a)
	if err != nil || len(ids) != 2 || ids[0] >= ids[1] {
		t.Fatalf("raw ids = %v err=%v, want 2 ordered COMPACTED_MSGS", ids, err)
	}
}
