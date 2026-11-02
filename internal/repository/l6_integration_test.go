//go:build integration

package repository_test

import (
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

// l6Exec 执行一条造数 SQL。
func l6Exec(t *testing.T, db *repository.DB, q string, args ...any) {
	t.Helper()
	if err := db.WithContext(t.Context()).Exec(q, args...).Error; err != nil {
		t.Fatal(err)
	}
}

// l6Ticket 给 run / order 插一张指定状态的票（digest 64 位，snapshot_raw_id 不校验外键）。
func l6Ticket(t *testing.T, db *repository.DB, runID, orderID uint64, status string, at time.Time) {
	t.Helper()
	l6Exec(t, db, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 1, 'LOW', '["agronomist"]', 1001, ?, 1, ?, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, runID, orderID, fmt.Sprintf("%064d", 0), status, at.Add(time.Hour), at, at)
}

// 验收 2：检查点落库即置 run INTERRUPTED、只留最近 3 份；非 RUNNING 不可再中断；Get 读最新一份。
func TestCheckpointRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	repo := repository.NewCheckpointRepo(slot.DB, slot.Clock, nil, nil, "")
	id := strconv.FormatUint(runID, 10)
	ctx := agent.WithCheckpointTarget(t.Context(), agent.CheckpointTarget{Scope: l4Scope, RunID: runID,
		Interrupt: model.InterruptWaitApproval})

	if err := repo.Set(t.Context(), id, []byte("x")); !errors.Is(err, repository.ErrBadCheckpoint) {
		t.Fatalf("set without target: %v", err)
	}
	if _, ok, err := repo.Get(ctx, id); err != nil || ok {
		t.Fatalf("get empty = %v, %v", ok, err)
	}
	for i := 1; i <= 4; i++ {
		l6Exec(t, slot.DB, `UPDATE agent_run SET status='RUNNING', interrupt_type=NULL WHERE id=?`, runID)
		if err := repo.Set(ctx, id, []byte("ckpt-"+strconv.Itoa(i))); err != nil {
			t.Fatalf("set %d: %v", i, err)
		}
	}
	if err := repo.Set(ctx, id, []byte("again")); !errors.Is(err, model.ErrIllegalTransition) {
		t.Fatalf("set on INTERRUPTED run: %v, want illegal transition", err)
	}
	got, ok, err := repo.Get(ctx, id)
	if err != nil || !ok || string(got) != "ckpt-4" {
		t.Fatalf("get = %q %v %v, want ckpt-4", got, ok, err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM run_checkpoint WHERE run_id=?", runID); n != 3 {
		t.Errorf("checkpoints kept = %d, want 3", n)
	}
	if n := itest.Count(t, slot.DB,
		"SELECT COUNT(*) FROM agent_run WHERE id=? AND status='INTERRUPTED' AND interrupt_type='WAIT_APPROVAL'", runID); n != 1 {
		t.Error("run not INTERRUPTED(WAIT_APPROVAL)")
	}
}

// MarkResumed：无 APPROVED 票 / 图版本不符不可续；通过后 RUNNING 且 epoch+1；RecheckEpoch 每次持久化 resume_epoch+1。
func TestMarkResumedAndRecheckEpoch_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	runs := repository.NewRunRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "APPROVED", t0)
	l6Exec(t, slot.DB, `UPDATE agent_run SET status='INTERRUPTED', interrupt_type='WAIT_APPROVAL' WHERE id=?`, runID)
	ctx := t.Context()
	req := agent.ResumeRequest{RunID: runID, GraphVersion: "g1"}

	if _, err := runs.MarkResumed(ctx, l4Scope, req); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("no approved ticket: %v", err)
	}
	l6Ticket(t, slot.DB, runID, orderID, "APPROVED", t0)
	if _, err := runs.MarkResumed(ctx, l4Scope, agent.ResumeRequest{RunID: runID, GraphVersion: "g2"}); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("graph version changed: %v", err)
	}
	epoch, err := runs.MarkResumed(ctx, l4Scope, req)
	if err != nil || epoch != 1 {
		t.Fatalf("resume = %d, %v; want epoch 1", epoch, err)
	}
	if _, err := runs.MarkResumed(ctx, l4Scope, req); !errors.Is(err, agent.ErrNotResumable) {
		t.Fatalf("resume twice: %v", err)
	}
	if re, err := runs.RecheckEpoch(ctx, l4Scope, runID); err != nil || re != 2 {
		t.Fatalf("recheck epoch = %d, %v; want 2", re, err)
	}
	// 再次复检分配新 epoch（持久化递增），时效工具 call_key 不与上一次复检相同
	if re, err := runs.RecheckEpoch(ctx, l4Scope, runID); err != nil || re != 3 {
		t.Fatalf("second recheck epoch = %d, %v; want 3", re, err)
	}
	if _, err := runs.RecheckEpoch(ctx, l4Scope, runID+100); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("recheck epoch of missing run: %v", err)
	}
}

// P1-L4 审查修复（中止关票）：LiveTicketsTx 取 OPEN 票 → SupersedeLiveTicketTx 置 SUPERSEDED（row_ver 守卫，旧 row_ver 再写
// → ErrConcurrentUpdate）；CloseWaitingRunTx 只收尾 INTERRUPTED(WAIT_APPROVAL) 的 run，再调返回 false 不报错；
// 作废后的过期票不再被 ListExpired（approval_expire）取到。
func TestCancelClosesTicket_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0)
	l6Exec(t, slot.DB, `UPDATE agent_run SET status='INTERRUPTED', interrupt_type='WAIT_APPROVAL' WHERE id=?`, runID)
	l6Ticket(t, slot.DB, runID, orderID, "OPEN", t0)
	sys := repository.NewSystemScope("approval_expire")
	after := t0.Add(2 * time.Hour) // 票据 expires_at = t0+1h
	if rows, err := repo.ListExpired(ctx, sys, after); err != nil || len(rows) != 1 {
		t.Fatalf("expired before cancel = %+v, %v", rows, err)
	}
	ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID}
	err := repo.InTx(ctx, func(tx *repository.DB) error {
		live, err := repo.LiveTicketsTx(ctx, tx, ref)
		if err != nil || len(live) != 1 {
			return fmt.Errorf("live tickets = %+v, %w", live, err)
		}
		if err := repo.SupersedeLiveTicketTx(ctx, tx, live[0], after); err != nil {
			return err
		}
		if err := repo.SupersedeLiveTicketTx(ctx, tx, live[0], after); !errors.Is(err, model.ErrConcurrentUpdate) {
			return fmt.Errorf("stale supersede: %v, want ErrConcurrentUpdate", err)
		}
		if closed, err := repo.CloseWaitingRunTx(ctx, tx, l4Scope, runID, model.ReasonCanceledByUser, after); err != nil || !closed {
			return fmt.Errorf("close waiting run = %t, %v", closed, err)
		}
		if closed, err := repo.CloseWaitingRunTx(ctx, tx, l4Scope, runID, model.ReasonCanceledByUser, after); err != nil || closed {
			return fmt.Errorf("close completed run = %t, %v; want false, nil", closed, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM approval_ticket WHERE order_id=? AND status='SUPERSEDED' AND row_ver=1`,
		orderID); n != 1 {
		t.Error("ticket not SUPERSEDED with row_ver+1")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM agent_run WHERE id=? AND status='COMPLETED' AND interrupt_type IS NULL
  AND reason_code='CANCELED_BY_USER'`, runID); n != 1 {
		t.Error("run not COMPLETED(CANCELED_BY_USER)")
	}
	if rows, err := repo.ListExpired(ctx, sys, after); err != nil || len(rows) != 0 {
		t.Fatalf("expired after cancel = %+v, %v; want none", rows, err)
	}
}

// P1-L4 审查修复（设备不可用推后上限）：Claim 读回入队时刻 created_at，领取续租不改它（下发器据此判断 device_wait_max）。
func TestOutboxClaimCreatedAt_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	enq := t0.Add(-30 * time.Minute)
	l6Exec(t, slot.DB, `INSERT INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, attempts,
  next_run_at, created_at, updated_at) VALUES (?, ?, 'order.dispatch', 1, 'order.dispatch:1:1:1', '{"order_id":1}', 'PENDING', 0, ?, ?, ?)`,
		itest.TenantID, itest.FarmID, t0, enq, enq)
	id := uint64(itest.Count(t, slot.DB, `SELECT id FROM outbox WHERE dedup_key='order.dispatch:1:1:1'`))
	msgs, err := repository.OutboxRepo{DB: slot.DB}.Claim(t.Context(), repository.NewSystemScope("outbox"), t0, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.ID == id {
			if !m.CreatedAt.UTC().Equal(enq) {
				t.Fatalf("created_at = %v, want %v", m.CreatedAt, enq)
			}
			return
		}
	}
	t.Fatalf("outbox %d not claimed: %+v", id, msgs)
}

// TransitionsByRun 按 id 顺序返回 run 名下作业单的迁移（state_machine_legal 的数据源）。
func TestTransitionsByRun_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0)
	for _, tr := range [][2]string{{"", "DRAFT"}, {"DRAFT", "PENDING_APPROVAL"}} {
		l6Exec(t, slot.DB, `INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status,
  actor_type, actor_id, created_at) VALUES (?, ?, ?, ?, ?, ?, 'agent', 0, ?)`,
			itest.TenantID, itest.FarmID, orderID, runID, tr[0], tr[1], t0)
	}
	got, err := repository.WorkOrderRepo{}.TransitionsByRun(t.Context(), slot.DB, l4Scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].To != model.OrderDraft || got[1].From != model.OrderDraft || got[1].To != model.OrderPendingApproval {
		t.Fatalf("transitions = %+v", got)
	}
	if other, err := (repository.WorkOrderRepo{}).TransitionsByRun(t.Context(), slot.DB, l4Scope, runID+1); err != nil || len(other) != 0 {
		t.Fatalf("other run = %+v, %v", other, err)
	}
}

// 验收 4：记忆读业务库：地块画像来自种子，episodic 按 occurred_at 倒序截 limit，他人农场不可见。
func TestMemoryRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repol6")
	slot := e.Fresh(t, t0)
	for i, kind := range []string{"ADVICE", "REJECTED", "ORDER_DONE"} {
		l6Exec(t, slot.DB, `INSERT INTO episodic_memory (tenant_id, farm_id, field_id, kind, ref_id, summary, occurred_at)
  VALUES (?, ?, 1, ?, ?, ?, ?)`, itest.TenantID, itest.FarmID, kind, i+1, kind+" summary", t0.Add(time.Duration(i)*time.Hour))
	}
	repo := repository.NewMemoryRepo(slot.DB)
	p, err := repo.Profile(t.Context(), l4Scope, 1)
	if err != nil || !p.Found || p.Summary == "" {
		t.Fatalf("profile = %+v, %v", p, err)
	}
	items, err := repo.Episodic(t.Context(), l4Scope, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != "ORDER_DONE" || items[1].Kind != "REJECTED" {
		t.Fatalf("episodic = %+v, want ORDER_DONE, REJECTED", items)
	}
	other := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 99}
	if p, err := repo.Profile(t.Context(), other, 1); err != nil || p.Found {
		t.Fatalf("other farm profile = %+v, %v", p, err)
	}
	if items, err := repo.Episodic(t.Context(), other, 1, 5); err != nil || len(items) != 0 {
		t.Fatalf("other farm episodic = %+v, %v", items, err)
	}
}
