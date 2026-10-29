//go:build integration

package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// memRaw 是内存 RawStore（集成测试只验 MySQL 侧，MinIO 由 raw 包自己的集成测试覆盖）。
type memRaw struct{ n uint64 }

func (m *memRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	m.n++
	return model.RawObject{ID: m.n, TenantID: p.TenantID, FarmID: p.FarmID, Kind: p.Kind}, nil
}

// §6.6 验收 3：开票四写同事务、重跑不重复；run 未中断时 Activate 被 JOIN 守卫拦下；中断后 OPEN 并进待审批列表。
func TestApproval_Integration(t *testing.T) {
	e := itest.Setup(t, "approval")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy", "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	orders, events := repository.WorkOrderRepo{}, repository.EventRepo{}
	svc, err := service.NewApprovalService(service.ApprovalDeps{Clock: slot.Clock, DB: slot.DB,
		Tx: repository.Transactor{DB: slot.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
		WorkOrders: service.NewWorkOrderService(slot.Clock, orders, events), Events: events, Raw: &memRaw{}, Risk: risk})
	if err != nil {
		t.Fatal(err)
	}
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "DRAFT", t0)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	req := agent.ApprovalRequest{InitiatorID: 1001,
		Order: model.WorkOrder{OrderID: orderID, AdviceID: runID, RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID,
			WorkType: model.WorkSpray, Status: model.OrderDraft, ConstraintChecks: []model.ConstraintCheck{{
				Phase: model.PhasePreApproval, RuleID: "kb#t1", Passed: true,
				Observed: []byte(`{"value":1,"tool_call_id":9}`), Threshold: []byte(`{"kb":"kb@1"}`)}}},
		Card: model.AdviceCard{AdviceID: runID, RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID}}

	first, err := svc.Open(ctx, scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.RiskLevel != model.RiskHigh || !first.ConfirmRequired {
		t.Errorf("SPRAY ticket %s confirm=%v, want HIGH + confirm", first.RiskLevel, first.ConfirmRequired)
	}
	for range 2 { // 重跑：返回同一张票，四张表行数不变
		again, err := svc.Open(ctx, scope, req)
		if err != nil || again.TicketID != first.TicketID {
			t.Fatalf("rerun ticket %d (%v), want %d", again.TicketID, err, first.TicketID)
		}
	}
	wantCounts(t, slot.DB, runID, orderID, map[string]int64{
		"SELECT COUNT(*) FROM approval_ticket WHERE order_id = ? AND ticket_seq = 1 AND status = 'PREPARING'": 1,
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL' AND attempt = 1": 1,
		"SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'PENDING_APPROVAL'":                        1,
	}, map[string]int64{"APPROVAL_OPEN": 1, "ORDER_TRANSITION": 1, "APPROVAL_PENDING": 0})

	// 并发开票在事务内命中 uk_order_seq：返回已有 id、created=false。
	err = repository.Transactor{DB: slot.DB}.InTx(ctx, func(tx *repository.DB) error {
		id, created, err := repository.ApprovalRepo{}.InsertTicketTx(ctx, tx, first, slot.Clock.Now())
		if err == nil && (created || id != first.TicketID) {
			t.Errorf("duplicate insert id=%d created=%v, want %d/false", id, created, first.TicketID)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Activate(ctx, scope, runID); !errors.Is(err, agent.ErrNotActivatable) {
		t.Fatalf("activate RUNNING run: err = %v, want ErrNotActivatable", err)
	}
	if list, err := svc.ListPending(ctx, scope); err != nil || len(list) != 0 {
		t.Fatalf("PREPARING ticket visible: %+v (%v)", list, err)
	}

	if err := slot.DB.WithContext(ctx).Exec(
		`UPDATE agent_run SET status='INTERRUPTED', interrupt_type='WAIT_APPROVAL' WHERE id=?`, runID).Error; err != nil {
		t.Fatal(err)
	}
	slot.Clock.Advance(time.Minute)
	opened, err := svc.Activate(ctx, scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := risk.TTL(model.RiskHigh)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Status != model.TicketOpen || opened.ExpiresAt == nil || !opened.ExpiresAt.Equal(slot.Clock.Now().Add(ttl)) {
		t.Errorf("activated %+v", opened)
	}
	if _, err := svc.Activate(ctx, scope, runID); err != nil { // 幂等
		t.Fatal(err)
	}
	wantCounts(t, slot.DB, runID, orderID, map[string]int64{
		"SELECT COUNT(*) FROM approval_ticket WHERE order_id = ? AND status = 'OPEN' AND expires_at IS NOT NULL": 1,
	}, map[string]int64{"APPROVAL_PENDING": 1})
	list, err := svc.ListPending(ctx, scope)
	if err != nil || len(list) != 1 || list[0].TicketID != first.TicketID || list[0].OrderID != orderID {
		t.Fatalf("pending list %+v (%v)", list, err)
	}
}

// wantCounts 校验以 order_id 为参数的行数与该 run 各类事件数。
func wantCounts(t *testing.T, db *repository.DB, runID, orderID uint64, rows map[string]int64, evs map[string]int64) {
	t.Helper()
	for q, want := range rows {
		if got := itest.Count(t, db, q, orderID); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	for typ, want := range evs {
		got := itest.Count(t, db, "SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = ?", runID, typ)
		if got != want {
			t.Errorf("run_event %s = %d, want %d", typ, got, want)
		}
	}
}
