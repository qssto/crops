package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type fakeDetailStore struct {
	row     repository.TicketRow
	rowErr  error
	orders  map[uint64]model.WorkOrder
	ordsErr error
}

func (f fakeDetailStore) TicketTx(context.Context, *repository.DB, model.Scope, uint64) (repository.TicketRow, error) {
	return f.row, f.rowErr
}

func (f fakeDetailStore) OrdersWithChecks(context.Context, model.Scope, []uint64) (map[uint64]model.WorkOrder, error) {
	return f.orders, f.ordsErr
}

type fakeSnapReader struct {
	body []byte
	err  error
}

func (f fakeSnapReader) Get(context.Context, model.Scope, uint64) ([]byte, error) {
	return f.body, f.err
}

func detailFixture(t *testing.T) (fakeDetailStore, fakeSnapReader) {
	t.Helper()
	area := 120.0
	snap := approvalSnapshot{TicketSeq: 1, RiskLevel: model.RiskHigh, RiskReasons: []string{model.RiskReasonPesticide, model.RiskReasonLargeArea},
		Highlights: &OrderHighlights{Product: json.RawMessage(`"吡虫啉"`), AreaMu: &area},
		Diff:       &OrderDiff{Note: DiffNoPrevious, Fields: []FieldDiff{}}}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	first := uint64(1002)
	row := repository.TicketRow{ID: 7, TenantID: 1, FarmID: 2, OrderID: 9, RiskLevel: string(model.RiskHigh), RequiredRole: `["agronomist"]`,
		ConfirmRequired: 1, FirstApproverID: &first, SnapshotDigest: hex.EncodeToString(sum[:]), SnapshotRawID: 501, Status: "OPEN"}
	checks := []model.ConstraintCheck{
		{Phase: model.PhasePreApproval, Attempt: 1, RuleID: "wind", Passed: true},
		{Phase: model.PhasePreDispatch, Attempt: 1, RuleID: "wind", Passed: false},
		{Phase: model.PhasePreDispatch, Attempt: 2, RuleID: "wind", Passed: true},
		{Phase: model.PhasePreDispatch, Attempt: 2, RuleID: "rain", Passed: true},
	}
	return fakeDetailStore{row: row, orders: map[uint64]model.WorkOrder{9: {OrderID: 9, ConstraintChecks: checks}}},
		fakeSnapReader{body: body}
}

// 详情：快照原样返回并解出命中原因 / 高亮 / 差异；校验行分 PRE_APPROVAL 与最新一轮 PRE_DISPATCH；HIGH 已一审 → awaiting_confirm。
func TestApprovalDetail_Assembles(t *testing.T) {
	store, raw := detailFixture(t)
	svc, err := NewApprovalDetailService(store, raw)
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.Detail(context.Background(), approvalScope, 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(d.Snapshot) != string(raw.body) || len(d.RiskReasons) != 2 || d.RiskReasons[1] != model.RiskReasonLargeArea ||
		d.Highlights == nil || *d.Highlights.AreaMu != 120 || d.Diff == nil || d.Diff.Note != DiffNoPrevious {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.PreApprovalChecks) != 1 || len(d.PreDispatchChecks) != 2 || d.PreDispatchChecks[0].Attempt != 2 {
		t.Fatalf("checks pre=%+v dispatch=%+v", d.PreApprovalChecks, d.PreDispatchChecks)
	}
	if !d.AwaitingConfirm || d.Ticket.TicketID != 7 || d.WorkOrder.OrderID != 9 {
		t.Fatalf("ticket = %+v awaiting = %v", d.Ticket, d.AwaitingConfirm)
	}
}

// 应失败：票不在本农场 → ErrNotInScope；快照 digest 不符 / 读不到 / 作业单缺失 → 完整性错误（不含 ErrNotFound）。
func TestApprovalDetail_Errors(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*fakeDetailStore, *fakeSnapReader)
		want       error
		notFound   bool
		ticketZero bool
	}{
		{name: "ticket not in scope", mutate: func(s *fakeDetailStore, _ *fakeSnapReader) { s.rowErr = model.ErrNotFound }, want: ErrNotInScope},
		{name: "digest mismatch", mutate: func(_ *fakeDetailStore, r *fakeSnapReader) { r.body = append(r.body, ' ') }, want: ErrSnapshotCorrupt},
		{name: "snapshot missing", mutate: func(_ *fakeDetailStore, r *fakeSnapReader) { r.err = model.ErrNotFound }},
		{name: "order missing", mutate: func(s *fakeDetailStore, _ *fakeSnapReader) { s.orders = map[uint64]model.WorkOrder{} }},
		{name: "zero ticket", ticketZero: true, want: ErrBadInput},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, raw := detailFixture(t)
			if tt.mutate != nil {
				tt.mutate(&store, &raw)
			}
			svc, err := NewApprovalDetailService(store, raw)
			if err != nil {
				t.Fatal(err)
			}
			id := uint64(7)
			if tt.ticketZero {
				id = 0
			}
			_, err = svc.Detail(context.Background(), approvalScope, id)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && errors.Is(err, model.ErrNotFound) {
				t.Fatalf("integrity error must not wrap ErrNotFound: %v", err)
			}
		})
	}
	if _, err := NewApprovalDetailService(nil, nil); err == nil {
		t.Fatal("want missing deps error")
	}
}

type fakeRejections struct {
	rows     []model.RejectionReason
	err      error
	calls    int
	from, to time.Time
}

func (f *fakeRejections) ListRejections(_ context.Context, _ model.Scope, from, to time.Time) ([]model.RejectionReason, error) {
	f.calls++
	f.from, f.to = from, to
	return f.rows, f.err
}

// 驳回导出：跨度上限（含边界）、from / to 缺失或倒置 → ErrBadInput 且不查库。
func TestRejectionExport_Window(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		from    time.Time
		to      time.Time
		wantErr bool
	}{
		{"exactly max span", from, from.Add(31 * 24 * time.Hour), false},
		{"over max span", from, from.Add(31*24*time.Hour + time.Second), true},
		{"to before from", from, from.Add(-time.Second), true},
		{"missing from", time.Time{}, from, true},
		{"missing to", from, time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeRejections{}
			svc, err := NewRejectionExportService(store, 31*24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.Export(context.Background(), approvalScope, tt.from, tt.to)
			if tt.wantErr != (err != nil) || (tt.wantErr && (!errors.Is(err, ErrBadInput) || store.calls != 0)) {
				t.Fatalf("err = %v calls = %d", err, store.calls)
			}
		})
	}
	if _, err := NewRejectionExportService(&fakeRejections{}, 0); err == nil {
		t.Fatal("want error for zero span")
	}
	store := &fakeRejections{err: errors.New("db down")}
	svc, _ := NewRejectionExportService(store, time.Hour)
	if _, err := svc.Export(context.Background(), approvalScope, from, from.Add(time.Minute)); err == nil || errors.Is(err, ErrBadInput) {
		t.Fatalf("store error = %v", err)
	}
}
