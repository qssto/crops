//go:build integration

package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// discardLog 丢弃日志（集成测试只验库中行）。
var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// memRaw 是内存 RawStore（集成测试只验 MySQL 侧，MinIO 由 raw 包自己的集成测试覆盖）。
type memRaw struct{ n uint64 }

func (m *memRaw) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	m.n++
	return model.RawObject{ID: m.n, TenantID: p.TenantID, FarmID: p.FarmID, Kind: p.Kind}, nil
}

// passReviewer 放行全部作业单（本测试只验开票四写；约束拦截见 TestApprovalBlocked_Integration）。
type passReviewer struct{}

func (passReviewer) Review(o model.WorkOrder) ([]model.ConstraintCheck, []string) {
	return o.ConstraintChecks, nil
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
		WorkOrders: service.NewWorkOrderService(slot.Clock, orders, events), Events: events, Raw: &memRaw{}, Risk: risk,
		Constraints: passReviewer{}, History: repository.LocalOrderRepo{DB: slot.DB},
		Notify: repository.NotifyOutboxRepo{}, Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "DRAFT", t0)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	req := sprayRequest(runID, orderID)

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
		id, created, err := repository.ApprovalRepo{}.InsertTicketTx(ctx, tx, first, nil, slot.Clock.Now())
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

// sprayRequest 与 itest.InsertOrder 的库中作业单一致（SPRAY / DRONE / DRY_RUN；P1-L3 审查 3：Open 按库中字段核对）。
func sprayRequest(runID, orderID uint64) agent.ApprovalRequest {
	return agent.ApprovalRequest{InitiatorID: 1001,
		Order: model.WorkOrder{OrderID: orderID, AdviceID: runID, RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID,
			WorkType: model.WorkSpray, DeviceType: "DRONE", DispatchMode: model.DispatchDryRun, Status: model.OrderDraft,
			ConstraintChecks: []model.ConstraintCheck{{
				Phase: model.PhasePreApproval, RuleID: "kb#t1", Passed: true,
				Observed: []byte(`{"value":1,"tool_call_id":9}`), Threshold: []byte(`{"kb":"kb@1"}`)}}},
		Card: model.AdviceCard{AdviceID: runID, RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID}}
}

// P1 L3：真实 Reviewer 拦截（必检项缺行）：同事务写 constraint_check(PRE_APPROVAL, attempt=1) 与 CONSTRAINT_BLOCKED
// （run_event + audit_log APPROVAL/FAILED，P1-L3 审查 4），不建票、不迁移，单留 DRAFT；重跑仍拦截、校验行不重复（uk_check），
// 每次拦截各留一条事件。
func TestApprovalBlocked_Integration(t *testing.T) {
	e := itest.Setup(t, "approval")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy", "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.Root, "policy", "regulation_map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	reg := constraint.Default()
	regs, err := constraint.ParseRegulationMap(b, reg)
	if err != nil {
		t.Fatal(err)
	}
	orders, events := repository.WorkOrderRepo{}, repository.EventRepo{}
	svc, err := service.NewApprovalService(service.ApprovalDeps{Clock: slot.Clock, DB: slot.DB,
		Tx: repository.Transactor{DB: slot.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
		WorkOrders: service.NewWorkOrderService(slot.Clock, orders, events), Events: events, Raw: &memRaw{}, Risk: risk,
		Constraints: constraint.NewReviewer(reg, regs), History: repository.LocalOrderRepo{DB: slot.DB},
		Notify: repository.NotifyOutboxRepo{}, Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "DRAFT", t0)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	var rows int64
	for i := range 2 {
		_, err := svc.Open(ctx, scope, sprayRequest(runID, orderID))
		if !errors.Is(err, agent.ErrConstraintBlocked) {
			t.Fatalf("open #%d: err = %v, want ErrConstraintBlocked", i, err)
		}
		n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL' AND attempt = 1`, orderID)
		if n == 0 || (i > 0 && n != rows) {
			t.Fatalf("open #%d: constraint_check rows %d (first %d)", i, n, rows)
		}
		rows = n
	}
	wantCounts(t, slot.DB, runID, orderID, map[string]int64{
		"SELECT COUNT(*) FROM approval_ticket WHERE order_id = ?":                                       0,
		"SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'DRAFT'":                             1,
		"SELECT COUNT(*) FROM work_order_transition WHERE order_id = ?":                                 0,
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND passed = 0 AND rule_id = 'kb#t1'": 0,
		blockedAuditSQL: 2,
	}, map[string]int64{"APPROVAL_OPEN": 0, "ORDER_TRANSITION": 0, "CONSTRAINT_BLOCKED": 2})
}

// blockedAuditSQL 数某作业单的 CONSTRAINT_BLOCKED 审计投影（APPROVAL / FAILED，target=order:{id}）。
const blockedAuditSQL = `SELECT COUNT(*) FROM audit_log WHERE action = 'APPROVAL' AND result = 'FAILED' AND target = CONCAT('order:', ?)`

// switchReviewer 按 blocked 决定是否拦截（原样返回请求的校验行），用于「拦截 → 修正 → 放行」。
type switchReviewer struct{ blocked []string }

func (r *switchReviewer) Review(o model.WorkOrder) ([]model.ConstraintCheck, []string) {
	return o.ConstraintChecks, r.blocked
}

// P1-L3 审查 2：拦截后修正再开票。同一 attempt 的 PRE_APPROVAL 校验行按 uk_check upsert 为通过行，本次未出现的 rule_id
// 被清掉；票、迁移、APPROVAL_OPEN 正常写入，拦截留下的 CONSTRAINT_BLOCKED 与审计行仍在。
func TestApprovalBlockedThenPasses_Integration(t *testing.T) {
	e := itest.Setup(t, "approval")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy", "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rv := &switchReviewer{blocked: []string{"kb#t2"}}
	orders, events := repository.WorkOrderRepo{}, repository.EventRepo{}
	svc, err := service.NewApprovalService(service.ApprovalDeps{Clock: slot.Clock, DB: slot.DB,
		Tx: repository.Transactor{DB: slot.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
		WorkOrders: service.NewWorkOrderService(slot.Clock, orders, events), Events: events, Raw: &memRaw{}, Risk: risk,
		Constraints: rv, History: repository.LocalOrderRepo{DB: slot.DB},
		Notify: repository.NotifyOutboxRepo{}, Logger: discardLog})
	if err != nil {
		t.Fatal(err)
	}
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "DRAFT", t0)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	req := sprayRequest(runID, orderID)
	pass := req.Order.ConstraintChecks[0]
	failed := pass
	failed.RuleID, failed.Passed = "kb#t2", false
	req.Order.ConstraintChecks = []model.ConstraintCheck{pass, failed}
	if _, err := svc.Open(ctx, scope, req); !errors.Is(err, agent.ErrConstraintBlocked) {
		t.Fatalf("first open: err = %v, want ErrConstraintBlocked", err)
	}
	wantCounts(t, slot.DB, runID, orderID, map[string]int64{
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL' AND attempt = 1": 2,
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND passed = 0 AND rule_id = 'kb#t2'":       1,
	}, map[string]int64{"CONSTRAINT_BLOCKED": 1})

	rv.blocked = nil
	slot.Clock.Advance(time.Minute)
	req.Order.ConstraintChecks = []model.ConstraintCheck{pass}
	got, err := svc.Open(ctx, scope, req)
	if err != nil || got.TicketSeq != 1 {
		t.Fatalf("second open = %+v, %v", got, err)
	}
	wantCounts(t, slot.DB, runID, orderID, map[string]int64{
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL' AND attempt = 1":           1,
		"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND attempt = 1 AND rule_id = 'kb#t1' AND passed = 1": 1,
		"SELECT COUNT(*) FROM approval_ticket WHERE order_id = ? AND ticket_seq = 1 AND status = 'PREPARING'":           1,
		"SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'PENDING_APPROVAL'":                                  1,
		blockedAuditSQL: 1,
	}, map[string]int64{"CONSTRAINT_BLOCKED": 1, "APPROVAL_OPEN": 1, "ORDER_TRANSITION": 1})
	// 复核第 2 条遗留：PRE_APPROVAL 行已是通过行，那次拦截的失败行（rule_id / observed / threshold）留在 CONSTRAINT_BLOCKED payload 里。
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = 'CONSTRAINT_BLOCKED'
  AND payload->>'$.checks[0].rule_id' = 'kb#t2' AND payload->>'$.checks[0].threshold.kb' = 'kb@1'
  AND payload->>'$.checks[0].observed.tool_call_id' = '9' AND JSON_LENGTH(payload->'$.checks') = 1`, runID); n != 1 {
		t.Errorf("CONSTRAINT_BLOCKED payload evidence rows = %d, want 1", n)
	}
}

// 复核第 21 条：同一张单的拦截（写失败行 kb#t2）与开票（写通过行 kb#t1）并发（go test -race）。两边事务首句都锁 work_order 行，
// 不应出现 1213 死锁或其他错误；无论谁先提交，最终只有一张票、PRE_APPROVAL 行只剩开票的通过行、单进 PENDING_APPROVAL，
// 拦截先提交则留一条 CONSTRAINT_BLOCKED，输给开票则回滚、返回库中的票。
func TestApprovalBlockVsOpenConcurrent_Integration(t *testing.T) {
	e := itest.Setup(t, "approval")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy", "risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	newSvc := func(rv service.ConstraintReviewer) *service.ApprovalService {
		orders, events := repository.WorkOrderRepo{}, repository.EventRepo{}
		svc, err := service.NewApprovalService(service.ApprovalDeps{Clock: slot.Clock, DB: slot.DB,
			Tx: repository.Transactor{DB: slot.DB}, Tickets: repository.ApprovalRepo{}, Orders: orders,
			WorkOrders: service.NewWorkOrderService(slot.Clock, orders, events), Events: events, Raw: &memRaw{}, Risk: risk,
			Constraints: rv, History: repository.LocalOrderRepo{DB: slot.DB},
			Notify: repository.NotifyOutboxRepo{}, Logger: discardLog})
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	blocker, opener := newSvc(&switchReviewer{blocked: []string{"kb#t2"}}), newSvc(passReviewer{})
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	for i := range 10 {
		runID := itest.InsertRun(t, slot.DB, t0)
		orderID := itest.InsertOrder(t, slot.DB, runID, "DRAFT", t0)
		openReq := sprayRequest(runID, orderID)
		blockReq := sprayRequest(runID, orderID)
		failed := blockReq.Order.ConstraintChecks[0]
		failed.RuleID, failed.Passed = "kb#t2", false
		blockReq.Order.ConstraintChecks = []model.ConstraintCheck{failed}

		var (
			wg                sync.WaitGroup
			start             = make(chan struct{})
			blockErr, openErr error
			blockT, openT     model.ApprovalTicket
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			blockT, blockErr = blocker.Open(ctx, scope, blockReq)
		}()
		go func() {
			defer wg.Done()
			<-start
			openT, openErr = opener.Open(ctx, scope, openReq)
		}()
		close(start)
		wg.Wait()

		if openErr != nil {
			t.Fatalf("order #%d open: %v", i, openErr)
		}
		blockedWon := errors.Is(blockErr, agent.ErrConstraintBlocked)
		if !blockedWon && (blockErr != nil || blockT.TicketID != openT.TicketID) {
			t.Fatalf("order #%d block = %+v, %v; want ErrConstraintBlocked or ticket %d", i, blockT, blockErr, openT.TicketID)
		}
		var wantBlocked int64
		if blockedWon {
			wantBlocked = 1
		}
		wantCounts(t, slot.DB, runID, orderID, map[string]int64{
			"SELECT COUNT(*) FROM approval_ticket WHERE order_id = ?":                                                                  1,
			"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL'":                                      1,
			"SELECT COUNT(*) FROM constraint_check WHERE order_id = ? AND phase = 'PRE_APPROVAL' AND rule_id = 'kb#t1' AND passed = 1": 1,
			"SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'PENDING_APPROVAL'":                                             1,
			blockedAuditSQL: wantBlocked,
		}, map[string]int64{"APPROVAL_OPEN": 1, "ORDER_TRANSITION": 1, "CONSTRAINT_BLOCKED": wantBlocked})
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
