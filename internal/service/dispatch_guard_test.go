package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// memTickets 是 DispatchTicketReader 的桩：按 id 存票，err 非空时读失败；staleAfter > 0 时第 staleAfter 次之后的读
// 一律返回 SUPERSEDED（模拟写入前校验之后、调设备之前票被 Reopen 作废）。
type memTickets struct {
	rows       map[uint64]repository.TicketRow
	err        error
	reads      int
	staleAfter int
}

// newApprovedTickets 是缺省夹具：票 7 属于单 3、已 APPROVED。
func newApprovedTickets() *memTickets {
	return &memTickets{rows: map[uint64]repository.TicketRow{
		7: {ID: 7, TenantID: 1, FarmID: 1, RunID: 5, OrderID: 3, TicketSeq: 1, Status: string(model.TicketApproved)}}}
}

func (m *memTickets) Ticket(_ context.Context, _ *repository.DB, _ model.Scope, id uint64) (repository.TicketRow, error) {
	m.reads++
	if m.err != nil {
		return repository.TicketRow{}, m.err
	}
	t, ok := m.rows[id]
	if !ok {
		return repository.TicketRow{}, fmt.Errorf("ticket %d: %w", id, model.ErrNotFound)
	}
	if m.staleAfter > 0 && m.reads > m.staleAfter {
		t.Status = string(model.TicketSuperseded)
	}
	return t, nil
}

// memAlerts 记录 Fire 的告警（并发安全）。
type memAlerts struct {
	mu  sync.Mutex
	got []model.Alert
}

func (m *memAlerts) Fire(_ context.Context, a model.Alert) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, a)
}

func (m *memAlerts) names() []model.AlertName {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.AlertName, 0, len(m.got))
	for _, a := range m.got {
		out = append(out, a.Name)
	}
	return out
}

func setTicketStatus(st model.TicketStatus) func(*dispatchFixture) {
	return func(f *dispatchFixture) {
		t := f.tickets.rows[7]
		t.Status = string(st)
		f.tickets.rows[7] = t
	}
}

// 红线：审批前设备调用。票非 APPROVED / 票不属本单 / 票不存在时，DRY_RUN、REAL 首发、REAL 续跑重发都不得调 Adapter，
// 且各发一条 pre_approval_device P0（order_id 下钻）；读票失败只退避、不告警；票正常时照常调用、不告警。
// DRY_RUN 与 REAL 首发在写入前（admitTicket）就拦下，消息丢弃（err=nil）；续跑与"写入前通过、调设备前作废"由
// guardDeviceCall 兜底，返回 ErrPreApprovalDevice。
func TestDispatch_PreApprovalDeviceGuard(t *testing.T) {
	type path int
	const (
		dryRun path = iota
		realFirst
		realResume
	)
	staleBeforeCall := func(f *dispatchFixture) { f.tickets.staleAfter = 2 } // admit、锁内复核通过，第 3 次（guard）作废
	cases := []struct {
		name    string
		path    path
		mutate  func(*dispatchFixture)
		wantErr error
		blocked bool
		alert   bool
	}{
		{name: "dry-run approved ticket passes", path: dryRun},
		{name: "dry-run ticket OPEN dropped", path: dryRun, mutate: setTicketStatus(model.TicketOpen),
			blocked: true, alert: true},
		{name: "dry-run ticket SUPERSEDED dropped", path: dryRun, mutate: setTicketStatus(model.TicketSuperseded),
			blocked: true, alert: true},
		{name: "dry-run ticket missing dropped", path: dryRun,
			mutate: func(f *dispatchFixture) { delete(f.tickets.rows, 7) }, blocked: true, alert: true},
		{name: "dry-run ticket of another order dropped", path: dryRun, mutate: func(f *dispatchFixture) {
			t := f.tickets.rows[7]
			t.OrderID = 99
			f.tickets.rows[7] = t
		}, blocked: true, alert: true},
		{name: "dry-run ticket read error retries without alert", path: dryRun,
			mutate: func(f *dispatchFixture) { f.tickets.err = errors.New("db down") }, wantErr: errAny, blocked: true},
		{name: "dry-run ticket superseded before call blocked by final guard", path: dryRun, mutate: staleBeforeCall,
			wantErr: ErrPreApprovalDevice, blocked: true, alert: true},
		{name: "real first approved ticket passes", path: realFirst},
		{name: "real first ticket REJECTED dropped", path: realFirst, mutate: setTicketStatus(model.TicketRejected),
			blocked: true, alert: true},
		{name: "real first ticket OPEN dropped", path: realFirst, mutate: setTicketStatus(model.TicketOpen),
			blocked: true, alert: true},
		{name: "real first ticket superseded before call blocked by final guard", path: realFirst, mutate: staleBeforeCall,
			wantErr: ErrPreApprovalDevice, blocked: true, alert: true},
		{name: "real resume approved ticket passes", path: realResume},
		{name: "real resume ticket EXPIRED blocked", path: realResume, mutate: setTicketStatus(model.TicketExpired),
			wantErr: ErrPreApprovalDevice, blocked: true, alert: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f *dispatchFixture
			var msg repository.OutboxMessage
			switch tc.path {
			case dryRun:
				f, msg = newDispatchFixture(t, model.OrderApproved, false), dispatchMsg(okDispatchPayload)
			case realFirst:
				f, msg = newRealFixture(t, model.OrderApproved), realMsg(1)
			case realResume:
				f, msg = newRealFixture(t, model.OrderDispatched), realMsg(1)
				f.seedAttempt(1, model.AttemptSent, "", "")
			}
			if tc.mutate != nil {
				tc.mutate(f)
			}
			err := f.svc.Dispatch(context.Background(), msg)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("Dispatch err = %v, want nil", err)
			case tc.wantErr == errAny && err == nil:
				t.Fatal("Dispatch err = nil, want an error")
			case tc.wantErr != nil && tc.wantErr != errAny && !errors.Is(err, tc.wantErr):
				t.Fatalf("Dispatch err = %v, want %v", err, tc.wantErr)
			}
			called := f.dev.dryRuns + f.real.dispatches
			if tc.blocked && called != 0 {
				t.Fatalf("adapter called %d times although guard should block", called)
			} else if !tc.blocked && called != 1 {
				t.Fatalf("adapter called %d times, want 1", called)
			}
			names := f.alerts.names()
			if !tc.alert {
				if len(names) != 0 {
					t.Fatalf("alerts = %v, want none", names)
				}
				return
			}
			if len(names) != 1 || names[0] != model.AlertPreApprovalDevice {
				t.Fatalf("alerts = %v, want [pre_approval_device]", names)
			}
			a := f.alerts.got[0]
			if a.Level != model.AlertP0 || a.OrderID != 3 || a.RunID != 5 || a.Scope.TenantID != 1 || a.Scope.FarmID != 1 {
				t.Fatalf("alert = %+v", a)
			}
		})
	}
}

// 审查修复（P1）：Reopen 后旧票 T1 已 SUPERSEDED、新票 T2（id 8）APPROVED。T1 的过时消息在任何写入前被拦下并丢弃：
// 单仍 APPROVED、无迁移 / DISPATCHED 事件 / SENT 尝试 / Archive / idem 键 / 复检；随后 T2 的合法消息照常下发。
func TestDispatch_StaleTicketDroppedBeforeWrite(t *testing.T) {
	cases := []struct {
		name string
		real bool
	}{
		{name: "dry-run"},
		{name: "real first", real: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispatchFixture(t, model.OrderApproved, false)
			if tc.real {
				f = newRealFixture(t, model.OrderApproved)
			}
			setTicketStatus(model.TicketSuperseded)(f)
			f.tickets.rows[8] = repository.TicketRow{ID: 8, TenantID: 1, FarmID: 1, RunID: 5, OrderID: 3, TicketSeq: 2,
				Status: string(model.TicketApproved)}
			if err := f.svc.Dispatch(context.Background(), realMsg(1)); err != nil {
				t.Fatalf("stale T1 Dispatch err = %v, want nil (message dropped)", err)
			}
			if f.store.order.Status != string(model.OrderApproved) || len(f.store.transitions) != 0 ||
				len(f.store.attempts) != 0 || len(f.events.events) != 0 {
				t.Fatalf("after block: order=%s transitions=%d attempts=%d events=%d, want unchanged", f.store.order.Status,
					len(f.store.transitions), len(f.store.attempts), len(f.events.events))
			}
			if f.real.archives != 0 || len(f.idem.m) != 0 || f.pre.calls != 0 || f.dev.dryRuns+f.real.dispatches != 0 {
				t.Fatalf("after block: archives=%d idem=%d prechecks=%d adapter=%d, want 0", f.real.archives, len(f.idem.m),
					f.pre.calls, f.dev.dryRuns+f.real.dispatches)
			}
			if names := f.alerts.names(); len(names) != 1 || names[0] != model.AlertPreApprovalDevice {
				t.Fatalf("alerts = %v, want [pre_approval_device]", names)
			}
			t2 := dispatchMsg(`{"order_id":3,"ticket_id":8,"run_id":5}`)
			t2.ID, t2.DedupKey = 71, DispatchIdemKey(3, 8, 1)
			if err := f.svc.Dispatch(context.Background(), t2); err != nil {
				t.Fatalf("legit T2 Dispatch err = %v", err)
			}
			wantStatus, wantCalls := string(model.OrderDone), f.dev.dryRuns
			if tc.real {
				wantStatus, wantCalls = string(model.OrderDispatched), f.real.dispatches
			}
			if f.store.order.Status != wantStatus || wantCalls != 1 || len(f.alerts.names()) != 1 {
				t.Fatalf("after T2: order=%s adapter calls=%d alerts=%v, want %s / 1 / one alert", f.store.order.Status,
					wantCalls, f.alerts.names(), wantStatus)
			}
			if tc.real && (len(f.store.attempts) != 1 || f.store.attempts[0].TicketID != 8) {
				t.Fatalf("after T2: attempts = %+v, want one attempt of ticket 8", f.store.attempts)
			}
		})
	}
}

// guardDeviceCall 的单状态分支：意图已落库后单须为 DISPATCHED，DRY_RUN 还要求尚无结果；票号为 0 也拦。
func TestGuardDeviceCall_OrderState(t *testing.T) {
	ref := model.OrderRef{TenantID: 1, FarmID: 1, OrderID: 3}
	cases := []struct {
		name   string
		status model.WorkOrderStatus
		result bool
		check  deviceCallCheck
		reason string // 空表示放行
	}{
		{name: "dispatched passes", status: model.OrderDispatched, check: deviceCallCheck{TicketID: 7, DryRun: true}},
		{name: "real dispatched with result passes", status: model.OrderDispatched, result: true,
			check: deviceCallCheck{TicketID: 7}},
		{name: "approved without intent blocked", status: model.OrderApproved, check: deviceCallCheck{TicketID: 7},
			reason: "order status APPROVED"},
		{name: "pending approval blocked", status: model.OrderPendingApproval, check: deviceCallCheck{TicketID: 7},
			reason: "order status PENDING_APPROVAL"},
		{name: "canceled blocked", status: model.OrderCanceled, check: deviceCallCheck{TicketID: 7, DryRun: true},
			reason: "order status CANCELED"},
		{name: "dry-run with result blocked", status: model.OrderDispatched, result: true,
			check: deviceCallCheck{TicketID: 7, DryRun: true}, reason: "dry-run result already recorded"},
		{name: "no ticket blocked", status: model.OrderDispatched, check: deviceCallCheck{}, reason: "no approval ticket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDispatchFixture(t, tc.status, false)
			if tc.result {
				f.store.result = []byte(`{}`)
			}
			_, err := f.svc.guardDeviceCall(context.Background(), ref, tc.check)
			if tc.reason == "" {
				if err != nil || len(f.alerts.got) != 0 {
					t.Fatalf("err = %v alerts = %v, want pass", err, f.alerts.names())
				}
				return
			}
			if !errors.Is(err, ErrPreApprovalDevice) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want ErrPreApprovalDevice with %q", err, tc.reason)
			}
			if len(f.alerts.got) != 1 || f.alerts.got[0].Name != model.AlertPreApprovalDevice || f.tickets.reads != 0 {
				t.Fatalf("alerts = %v ticket reads = %d", f.alerts.names(), f.tickets.reads)
			}
		})
	}
}

// 未装配票据读取时 SetDispatch 失败关闭。
func TestSetDispatch_RequiresTickets(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	d := f.deps
	d.Tickets = nil
	if err := NewWorkOrderService(fixedClock{dispatchNow}, f.store, f.events).SetDispatch(d); !errors.Is(err,
		ErrDispatchNotConfigured) {
		t.Fatalf("SetDispatch err = %v, want ErrDispatchNotConfigured", err)
	}
}
