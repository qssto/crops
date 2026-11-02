package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// fakeReopen 是内存 ReopenStore：superseded 记录守卫通过的旧票；supersedeErr 模拟旧票 row_ver 冲突；
// queued 按 dedup_key 去重（INSERT IGNORE 语义）。
type fakeReopen struct {
	ticket       repository.TicketRow
	supersedeErr error
	superseded   []repository.TicketRow
	inserted     []model.ApprovalTicket
	expires      time.Time
	queued       map[string][]byte
	notifyTries  int
}

func (f *fakeReopen) Ticket(context.Context, *repository.DB, model.Scope, uint64) (repository.TicketRow, error) {
	return f.ticket, nil
}

func (f *fakeReopen) SupersedeTicketTx(_ context.Context, _ *repository.DB, t repository.TicketRow, _ time.Time) error {
	if f.supersedeErr != nil {
		return f.supersedeErr
	}
	f.superseded = append(f.superseded, t)
	return nil
}

func (f *fakeReopen) InsertReopenedTicketTx(_ context.Context, _ *repository.DB, t model.ApprovalTicket, expiresAt,
	_ time.Time) (uint64, error) {
	f.inserted = append(f.inserted, t)
	f.expires = expiresAt
	return 88, nil
}

func (f *fakeReopen) InsertNotifyOutboxTx(_ context.Context, _ *repository.DB, _ model.OrderRef, key string, payload []byte,
	_ time.Time) (bool, error) {
	f.notifyTries++
	if f.queued == nil {
		f.queued = map[string][]byte{}
	}
	if _, dup := f.queued[key]; dup {
		return false, nil
	}
	f.queued[key] = payload
	return true, nil
}

func approvedTicketRow() repository.TicketRow {
	return repository.TicketRow{ID: 70, TenantID: 1, FarmID: 2, RunID: 5, OrderID: 9, TicketSeq: 1, RiskLevel: string(model.RiskHigh),
		RequiredRole: `["agronomist"]`, ConfirmRequired: 1, InitiatorID: 1002, SnapshotDigest: "old", SnapshotRawID: 400,
		Status: string(model.TicketApproved), RowVer: 4}
}

func reopenRequest() ReopenRequest {
	req := openRequest()
	req.Order.Status = model.OrderApproved
	req.Order.ConstraintChecks = []model.ConstraintCheck{{Phase: model.PhasePreDispatch, RuleID: "kb#wind", Passed: false}}
	return ReopenRequest{TicketID: 70, Order: req.Order, Card: req.Card, RuleIDs: []string{"kb#wind"}}
}

func newReopenFixture(t *testing.T, store *fakeReopen) (approvalFixture, *ReopenService) {
	t.Helper()
	f := newApprovalFixture(t, &fakeTickets{}, model.OrderApproved, fakeRisk{ttl: 2 * time.Hour})
	svc, err := NewReopenService(f.svc, store)
	if err != nil {
		t.Fatal(err)
	}
	return f, svc
}

// 退回成功：旧票 SUPERSEDED、单 APPROVED → PENDING_APPROVAL(RECHECK_FAILED)、新票 seq+1 OPEN 带新快照与 expires_at、
// PRE_APPROVAL 行 attempt=新 seq、notify 入队、APPROVAL_OPEN(actor=system, reopened_from)。
func TestReopen_ReturnsToApproval(t *testing.T) {
	store := &fakeReopen{ticket: approvedTicketRow()}
	f, svc := newReopenFixture(t, store)
	plan, err := svc.Prepare(context.Background(), reopenRequest())
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Reopen(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.superseded) != 1 || store.superseded[0].RowVer != 4 {
		t.Fatalf("superseded = %+v, want old ticket with row_ver 4", store.superseded)
	}
	if len(f.orders.transitions) != 1 || f.orders.transitions[0].To != model.OrderPendingApproval ||
		f.orders.transitions[0].ReasonCode != model.ReasonRecheckFailed {
		t.Fatalf("transitions = %+v", f.orders.transitions)
	}
	nt := store.inserted[0]
	if nt.TicketSeq != 2 || nt.Status != model.TicketOpen || nt.InitiatorID != 1002 || nt.Snapshot.RawObjectID != 501 ||
		nt.Snapshot.Digest == "old" || !store.expires.Equal(approvalNow.Add(2*time.Hour)) {
		t.Fatalf("new ticket = %+v expires %s", nt, store.expires)
	}
	if got.TicketID != 88 || got.ExpiresAt == nil {
		t.Fatalf("returned ticket = %+v", got)
	}
	if f.tickets.checkAttempt != 2 || f.tickets.trace[0] != "lock_order" {
		t.Fatalf("checks attempt = %d trace = %v", f.tickets.checkAttempt, f.tickets.trace)
	}
	if _, ok := store.queued[RecheckReturnedDedupKey(9, 70)]; !ok {
		t.Fatalf("notify not queued: %v", store.queued)
	}
	last := f.events.events[len(f.events.events)-1]
	var p model.ReopenedPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if last.Type != model.EventApprovalOpen || last.Actor != model.ActorSystem || p.ReopenedFrom != 70 || p.TicketID != 88 {
		t.Fatalf("event = %s %s %+v", last.Type, last.Actor, p)
	}
	if len(f.raw.puts) != 1 {
		t.Fatalf("snapshot puts = %d", len(f.raw.puts))
	}
}

// 旧票 row_ver 冲突：Supersede 返回 ErrConcurrentUpdate，事务回滚，迁移 / 新票 / 通知 / 事件一条都不写
// （应失败：冲突后仍迁移或插入新票）。
func TestReopen_SupersedeConflictRollsBack(t *testing.T) {
	store := &fakeReopen{ticket: approvedTicketRow(), supersedeErr: model.ErrConcurrentUpdate}
	f, svc := newReopenFixture(t, store)
	plan, err := svc.Prepare(context.Background(), reopenRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Reopen(context.Background(), plan)
	if !errors.Is(err, model.ErrConcurrentUpdate) || !errors.Is(f.tx.err, model.ErrConcurrentUpdate) {
		t.Fatalf("err = %v tx err = %v", err, f.tx.err)
	}
	if len(f.orders.transitions) != 0 || f.orders.updated || len(store.inserted) != 0 || store.notifyTries != 0 ||
		len(f.events.events) != 0 || f.tickets.upserts != 0 {
		t.Fatalf("wrote after conflict: transitions=%d inserted=%d notify=%d events=%d upserts=%d",
			len(f.orders.transitions), len(store.inserted), store.notifyTries, len(f.events.events), f.tickets.upserts)
	}
}

// 通知去重：同一旧票的 dedup_key 已在 outbox，INSERT IGNORE 不再新增，退回本身照常提交（应失败：重复入队或因此回滚）。
func TestReopen_NotifyDedup(t *testing.T) {
	store := &fakeReopen{ticket: approvedTicketRow(), queued: map[string][]byte{RecheckReturnedDedupKey(9, 70): []byte(`{}`)}}
	f, svc := newReopenFixture(t, store)
	plan, err := svc.Prepare(context.Background(), reopenRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reopen(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(store.queued) != 1 || string(store.queued[RecheckReturnedDedupKey(9, 70)]) != `{}` || store.notifyTries != 1 {
		t.Fatalf("queued = %v tries = %d", store.queued, store.notifyTries)
	}
	if f.tx.err != nil || len(store.inserted) != 1 {
		t.Fatalf("tx err = %v inserted = %d", f.tx.err, len(store.inserted))
	}
}

// 旧票已不是 APPROVED（已退回过 / 属别的单）：Prepare 拒绝，不写快照（应失败：照样换票）。
func TestReopen_PrepareRejectsNonApproved(t *testing.T) {
	for name, mutate := range map[string]func(*repository.TicketRow){
		"superseded":  func(r *repository.TicketRow) { r.Status = string(model.TicketSuperseded) },
		"other order": func(r *repository.TicketRow) { r.OrderID = 10 },
	} {
		t.Run(name, func(t *testing.T) {
			row := approvedTicketRow()
			mutate(&row)
			f, svc := newReopenFixture(t, &fakeReopen{ticket: row})
			if _, err := svc.Prepare(context.Background(), reopenRequest()); !errors.Is(err, ErrBadApproval) {
				t.Fatalf("err = %v", err)
			}
			if len(f.raw.puts) != 0 {
				t.Fatalf("snapshot written: %d", len(f.raw.puts))
			}
		})
	}
}
