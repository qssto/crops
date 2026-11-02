package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// snapshotOf 解出 fixture 最近一次写入的审批快照。
func snapshotOf(t *testing.T, f approvalFixture) approvalSnapshot {
	t.Helper()
	if len(f.raw.puts) == 0 {
		t.Fatal("no snapshot put")
	}
	var s approvalSnapshot
	if err := json.Unmarshal(f.raw.puts[len(f.raw.puts)-1].Body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// 开票：命中原因写票（InsertTicketTx）与快照；高亮取 payload；没有上一张同类单时差异为空并注明。
func TestApprovalOpen_RiskReasonsAndNoPreviousDiff(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	req := openRequest()
	req.Order.Payload = json.RawMessage(`{"product":"吡虫啉","dose_per_mu":{"value":10,"unit":"g"},"area_mu":30}`)
	if _, err := f.svc.Open(context.Background(), approvalScope, req); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.tickets.reasons, []string{model.RiskReasonPesticide}) {
		t.Fatalf("InsertTicketTx reasons = %v", f.tickets.reasons)
	}
	s := snapshotOf(t, f)
	if !slices.Equal(s.RiskReasons, []string{model.RiskReasonPesticide}) {
		t.Errorf("snapshot reasons = %v", s.RiskReasons)
	}
	if s.Highlights == nil || string(s.Highlights.Product) != `"吡虫啉"` || s.Highlights.AreaMu == nil || *s.Highlights.AreaMu != 30 ||
		string(s.Highlights.DosePerMu) != `{"value":10,"unit":"g"}` {
		t.Errorf("highlights = %+v", s.Highlights)
	}
	if s.Diff == nil || s.Diff.Note != DiffNoPrevious || s.Diff.PrevOrderID != 0 || len(s.Diff.Fields) != 0 {
		t.Errorf("diff = %+v, want empty with note", s.Diff)
	}
	if want := approvalNow.Add(-diffLookback); !f.history.since.Equal(want) {
		t.Errorf("history since = %v, want %v", f.history.since, want)
	}
}

// 差异：只比同类 DONE / PARTIALLY_DONE 单，取 updated_at 最新者；逐个顶层字段比较，相同字段不列出。
func TestApprovalOpen_DiffWithPreviousSameTypeOrder(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	at := func(h int) time.Time { return approvalNow.Add(-time.Duration(h) * time.Hour) }
	f.history.orders = []agent.LocalOrder{
		{OrderID: 1, WorkType: model.WorkSpray, Status: model.OrderDone, UpdatedAt: at(48), Payload: json.RawMessage(`{"product":"旧"}`)},
		{OrderID: 2, WorkType: model.WorkSpray, Status: model.OrderPartiallyDone, UpdatedAt: at(24),
			Payload: json.RawMessage(`{"product":"吡虫啉", "dose_per_mu": {"value":8,"unit":"g"}, "nozzle":"A"}`)},
		{OrderID: 3, WorkType: model.WorkIrrigate, Status: model.OrderDone, UpdatedAt: at(1), Payload: json.RawMessage(`{}`)},
		{OrderID: 4, WorkType: model.WorkSpray, Status: model.OrderExecuting, UpdatedAt: at(1), Payload: json.RawMessage(`{}`)},
	}
	req := openRequest()
	req.Order.Payload = json.RawMessage(`{"product":"吡虫啉","dose_per_mu":{"value":10,"unit":"g"},"area_mu":30}`)
	if _, err := f.svc.Open(context.Background(), approvalScope, req); err != nil {
		t.Fatal(err)
	}
	d := snapshotOf(t, f).Diff
	if d == nil || d.PrevOrderID != 2 || d.PrevStatus != string(model.OrderPartiallyDone) || d.Note != "" {
		t.Fatalf("diff = %+v, want prev order 2", d)
	}
	var got []string
	for _, fd := range d.Fields {
		got = append(got, fd.Field+":"+string(fd.Prev)+"→"+string(fd.Cur))
	}
	want := []string{`area_mu:null→30`, `dose_per_mu:{"value":8,"unit":"g"}→{"value":10,"unit":"g"}`, `nozzle:"A"→null`}
	if !slices.Equal(got, want) {
		t.Fatalf("diff fields = %v, want %v", got, want)
	}
}

// 应失败：payload 不是对象 → ErrBadApproval；差异读取失败 → 不建票。
func TestApprovalOpen_L5Errors(t *testing.T) {
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	req := openRequest()
	req.Order.Payload = json.RawMessage(`[1]`)
	if _, err := f.svc.Open(context.Background(), approvalScope, req); !errors.Is(err, ErrBadApproval) {
		t.Fatalf("bad payload err = %v", err)
	}
	f = newApprovalFixture(t, &fakeTickets{}, model.OrderDraft, fakeRisk{})
	f.history.err = errors.New("db down")
	if _, err := f.svc.Open(context.Background(), approvalScope, openRequest()); err == nil || f.tx.calls != 0 {
		t.Fatalf("history error: err = %v, tx calls = %d", err, f.tx.calls)
	}
}

// Activate 同事务写 notify.send(PENDING_APPROVAL)，dedup_key 按票；写失败整事务失败。
func TestApprovalActivate_NotifiesPending(t *testing.T) {
	tk := model.ApprovalTicket{TicketID: 77, RunID: 5, TenantID: 1, FarmID: 2, OrderID: 9, RiskLevel: model.RiskHigh, Status: model.TicketPreparing}
	f := newApprovalFixture(t, &fakeTickets{byRun: &tk, activated: 1}, model.OrderPendingApproval, fakeRisk{ttl: 2 * time.Hour})
	if _, err := f.svc.Activate(context.Background(), approvalScope, 5); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.notify.keys, []string{"notify:PENDING_APPROVAL:9:77"}) {
		t.Fatalf("notify keys = %v", f.notify.keys)
	}
	var env model.NotifyEnvelope
	if err := json.Unmarshal(f.notify.payloads[0], &env); err != nil ||
		env != (model.NotifyEnvelope{Kind: model.NotifyPendingApproval, OrderID: 9, RunID: 5, TicketID: 77}) {
		t.Fatalf("envelope = %+v (%v)", env, err)
	}

	f = newApprovalFixture(t, &fakeTickets{byRun: &tk, activated: 1}, model.OrderPendingApproval, fakeRisk{ttl: 2 * time.Hour})
	f.notify.err = errors.New("outbox down")
	if _, err := f.svc.Activate(context.Background(), approvalScope, 5); err == nil || len(f.events.events) != 0 {
		t.Fatalf("notify failure: err = %v, events = %d", err, len(f.events.events))
	}
}
