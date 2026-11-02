//go:build integration

package repository_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// l5Ticket 插一张 OPEN 的 HIGH 票（confirm_required=1；firstApprover 为 0 时 first_approver_id 写 NULL），返回票据行。
func l5Ticket(t *testing.T, repo *repository.DecisionRepo, db *repository.DB, runID, orderID, firstApprover uint64,
	at time.Time) repository.TicketRow {
	t.Helper()
	var first any
	if firstApprover != 0 {
		first = firstApprover
	}
	l6Exec(t, db, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  confirm_required, risk_reasons, initiator_id, first_approver_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 1, 'HIGH', '["agronomist"]', 1, '["PESTICIDE","LARGE_AREA"]', 1001, ?, ?, 1, 'OPEN', ?, ?, ?)`,
		itest.TenantID, itest.FarmID, runID, orderID, first, fmt.Sprintf("%064d", 0), at.Add(30*time.Minute), at, at)
	id := uint64(itest.Count(t, db, `SELECT MAX(id) FROM approval_ticket WHERE order_id=?`, orderID))
	row, err := repo.TicketTx(t.Context(), db, l4Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// P1 L5 第 2 条（红线）：第二审批人生效 UPDATE 只在 first_approver_id 非空且不是本人时命中；
// 应失败：无第一审批人 / 同一人确认 / 旧 row_ver → ErrConcurrentUpdate，票据保持 OPEN。
func TestConfirmTicketTxGuard_Integration(t *testing.T) {
	e := itest.Setup(t, "repol5")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	confirm := func(row repository.TicketRow, operator uint64) error {
		return repo.InTx(ctx, func(tx *repository.DB) error { return repo.ConfirmTicketTx(ctx, tx, row, operator, t0) })
	}
	runA := itest.InsertRun(t, slot.DB, t0)
	noFirst := l5Ticket(t, repo, slot.DB, runA, itest.InsertOrder(t, slot.DB, runA, "PENDING_APPROVAL", t0), 0, t0)
	if err := confirm(noFirst, 1004); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("confirm without first approver = %v, want ErrConcurrentUpdate", err)
	}
	runB := itest.InsertRun(t, slot.DB, t0)
	row := l5Ticket(t, repo, slot.DB, runB, itest.InsertOrder(t, slot.DB, runB, "PENDING_APPROVAL", t0), 1002, t0)
	if err := confirm(row, 1002); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("confirm by first approver = %v, want ErrConcurrentUpdate", err)
	}
	stale := row
	stale.RowVer++
	if err := confirm(stale, 1004); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("confirm with stale row_ver = %v, want ErrConcurrentUpdate", err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM approval_ticket WHERE id IN (?, ?) AND status='OPEN'`,
		noFirst.ID, row.ID); n != 2 {
		t.Fatalf("open tickets after denied confirms = %d, want 2", n)
	}
	if err := confirm(row, 1004); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM approval_ticket WHERE id=? AND status='APPROVED' AND operator_id=1004
  AND first_approver_id=1002 AND row_ver=?`, row.ID, row.RowVer+1); n != 1 {
		t.Error("ticket not APPROVED by second approver with row_ver+1")
	}
}

// P1 L5 第 3 / 4 条：驳回原因随驳回写入（order / advice / run 取自 work_order，一票一行），导出按时间窗读回；
// 应失败：作业单不存在 → ErrConcurrentUpdate；同票再写 → uk_ticket 冲突；窗外不导出。
func TestRejectionInsertAndExport_Integration(t *testing.T) {
	e := itest.Setup(t, "repol5")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0)
	row := l5Ticket(t, repo, slot.DB, runID, orderID, 0, t0)
	text := "用量偏高"
	in := repository.RejectionInput{ReasonCode: model.RejectDosage, ReasonText: &text, OperatorID: 1002,
		OperatorRole: model.RoleAgronomist}
	insert := func(r repository.TicketRow) error {
		return repo.InTx(ctx, func(tx *repository.DB) error { return repo.InsertRejectionTx(ctx, tx, r, in, t0) })
	}
	if err := insert(row); err != nil {
		t.Fatal(err)
	}
	if err := insert(row); err == nil {
		t.Fatal("second rejection for the same ticket inserted, want uk_ticket conflict")
	}
	missing := row
	missing.OrderID = orderID + 1000
	if err := insert(missing); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("rejection without work_order = %v, want ErrConcurrentUpdate", err)
	}
	exp := repository.RejectionRepo{DB: slot.DB}
	got, err := exp.ListRejections(ctx, l4Scope, t0.Add(-time.Minute), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TicketID != row.ID || got[0].OrderID != orderID || got[0].AdviceID != runID ||
		got[0].RunID != runID || got[0].ReasonCode != model.RejectDosage || got[0].ReasonText == nil ||
		*got[0].ReasonText != text || got[0].OperatorRole != model.RoleAgronomist {
		t.Fatalf("rejections = %+v", got)
	}
	if got, err := exp.ListRejections(ctx, l4Scope, t0.Add(time.Hour), t0.Add(2*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("rejections outside window = %+v, %v; want none", got, err)
	}
}

// P1 L5 第 5 条：即将过期扫描只取 expires_at ∈ [now, now+1h] 的 OPEN 票；已过期 / 1h 之外 / 非 OPEN 不取。
func TestListExpiring_Integration(t *testing.T) {
	e := itest.Setup(t, "repol5")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	ticket := func(status string, expires time.Time) uint64 {
		runID := itest.InsertRun(t, slot.DB, t0)
		orderID := itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0)
		l6Exec(t, slot.DB, `INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 1, 'LOW', '["agronomist"]', 1001, ?, 1, ?, ?, ?, ?)`,
			itest.TenantID, itest.FarmID, runID, orderID, fmt.Sprintf("%064d", 0), status, expires, t0, t0)
		return uint64(itest.Count(t, slot.DB, `SELECT MAX(id) FROM approval_ticket WHERE order_id=?`, orderID))
	}
	soon := ticket("OPEN", t0.Add(30*time.Minute))
	ticket("OPEN", t0.Add(-time.Minute))       // 已过期：归 approval_expire
	ticket("OPEN", t0.Add(2*time.Hour))        // 1h 之外
	ticket("APPROVED", t0.Add(30*time.Minute)) // 已决策
	if _, err := repo.ListExpiring(ctx, repository.SystemScope{}, t0); err == nil {
		t.Fatal("ListExpiring with zero SystemScope succeeded, want error")
	}
	rows, err := repo.ListExpiring(ctx, repository.NewSystemScope("approval_expiring"), t0)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uint64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []uint64{soon}) {
		t.Fatalf("expiring tickets = %v, want [%d]", ids, soon)
	}
}

// 评测观测（approval_gate）：TicketRiskReasons 读回 JSON 数组、NULL 为 nil；TicketDeniedCodes 按写入顺序读 DENIED 码。
func TestTicketGateObservations_Integration(t *testing.T) {
	e := itest.Setup(t, "repol5")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewDecisionRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	row := l5Ticket(t, repo, slot.DB, runID, itest.InsertOrder(t, slot.DB, runID, "PENDING_APPROVAL", t0), 0, t0)
	ar := repository.ApprovalRepo{}
	reasons, err := ar.TicketRiskReasons(ctx, slot.DB, l4Scope, row.ID)
	if err != nil || !slices.Equal(reasons, []string{model.RiskReasonPesticide, model.RiskReasonLargeArea}) {
		t.Fatalf("risk reasons = %v, %v", reasons, err)
	}
	if _, err := ar.TicketRiskReasons(ctx, slot.DB, l4Scope, row.ID+1000); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing ticket risk reasons err = %v, want ErrNotFound", err)
	}
	if codes, err := ar.TicketDeniedCodes(ctx, slot.DB, l4Scope, row.ID); err != nil || len(codes) != 0 {
		t.Fatalf("denied codes before = %v, %v; want empty", codes, err)
	}
	for _, code := range []string{"ROLE_INSUFFICIENT", "SAME_APPROVER"} {
		if err := repo.AuditDenied(ctx, l4Scope, row, code); err != nil {
			t.Fatal(err)
		}
	}
	codes, err := ar.TicketDeniedCodes(ctx, slot.DB, l4Scope, row.ID)
	if err != nil || !slices.Equal(codes, []string{"ROLE_INSUFFICIENT", "SAME_APPROVER"}) {
		t.Fatalf("denied codes = %v, %v", codes, err)
	}
}
