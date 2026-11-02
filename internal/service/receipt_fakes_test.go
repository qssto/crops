package service

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// receiptWorld 是回执侧的内存库：ReceiptStore + ReceiptLookup + OrderTransitioner + TxRunner 同一份状态，
// InTx 失败整体回滚（模拟单事务），供 Process / Webhook / 扫描 / 对账 / 中止的表驱动用例使用。
type receiptWorld struct {
	mu          sync.Mutex
	orders      map[uint64]repository.DispatchOrder
	doneArea    map[uint64]float64
	attempts    map[uint64]model.DispatchAttempt
	receipts    map[string]repository.ReceiptRow
	outbox      []rwOutbox
	memories    map[uint64]string
	transitions []rwTransition
	tickets     map[uint64]repository.TicketRow // 中止关票（CancelTicketStore）
	waitingRuns map[uint64]bool                 // run 是否仍 INTERRUPTED(WAIT_APPROVAL)
	closedRuns  map[uint64]model.ReasonCode
	txCalls     int
	failTrans   error // 非空时 Transition 失败（测回滚）
	lookupErr   error
}

type rwOutbox struct {
	Topic   string
	Dedup   string
	Payload []byte
}

type rwTransition struct {
	OrderID  uint64
	From, To model.WorkOrderStatus
	Actor    model.Actor
	Reason   model.ReasonCode
}

func newReceiptWorld() *receiptWorld {
	return &receiptWorld{orders: map[uint64]repository.DispatchOrder{}, doneArea: map[uint64]float64{},
		attempts: map[uint64]model.DispatchAttempt{}, receipts: map[string]repository.ReceiptRow{}, memories: map[uint64]string{},
		tickets: map[uint64]repository.TicketRow{}, waitingRuns: map[uint64]bool{}, closedRuns: map[uint64]model.ReasonCode{}}
}

type rwSnap struct {
	orders      map[uint64]repository.DispatchOrder
	doneArea    map[uint64]float64
	attempts    map[uint64]model.DispatchAttempt
	receipts    map[string]repository.ReceiptRow
	outbox      []rwOutbox
	memories    map[uint64]string
	transitions []rwTransition
	tickets     map[uint64]repository.TicketRow
	waitingRuns map[uint64]bool
	closedRuns  map[uint64]model.ReasonCode
}

func (w *receiptWorld) snap() rwSnap {
	return rwSnap{maps.Clone(w.orders), maps.Clone(w.doneArea), maps.Clone(w.attempts), maps.Clone(w.receipts),
		slices.Clone(w.outbox), maps.Clone(w.memories), slices.Clone(w.transitions), maps.Clone(w.tickets),
		maps.Clone(w.waitingRuns), maps.Clone(w.closedRuns)}
}

func (w *receiptWorld) restore(s rwSnap) {
	w.orders, w.doneArea, w.attempts, w.receipts = s.orders, s.doneArea, s.attempts, s.receipts
	w.outbox, w.memories, w.transitions = s.outbox, s.memories, s.transitions
	w.tickets, w.waitingRuns, w.closedRuns = s.tickets, s.waitingRuns, s.closedRuns
}

// LiveTicketsTx 返回作业单上 OPEN / PREPARING 的票。
func (w *receiptWorld) LiveTicketsTx(_ context.Context, _ *repository.DB, ref model.OrderRef) ([]repository.TicketRow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []repository.TicketRow
	for _, t := range w.tickets {
		st := model.TicketStatus(t.Status)
		if sameFarm(ref, t.TenantID, t.FarmID) && t.OrderID == ref.OrderID && (st == model.TicketOpen || st == model.TicketPreparing) {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(x, y repository.TicketRow) int { return int(x.ID) - int(y.ID) })
	return out, nil
}

// SupersedeLiveTicketTx 照 SQL 守卫：OPEN / PREPARING 且 row_ver 一致才改 SUPERSEDED。
func (w *receiptWorld) SupersedeLiveTicketTx(_ context.Context, _ *repository.DB, t repository.TicketRow, _ time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.tickets[t.ID]
	st := model.TicketStatus(cur.Status)
	if !ok || cur.RowVer != t.RowVer || (st != model.TicketOpen && st != model.TicketPreparing) {
		return model.ErrConcurrentUpdate
	}
	cur.Status, cur.RowVer = string(model.TicketSuperseded), cur.RowVer+1
	w.tickets[t.ID] = cur
	return nil
}

// CloseWaitingRunTx 只收尾仍等审批的 run。
func (w *receiptWorld) CloseWaitingRunTx(_ context.Context, _ *repository.DB, _ model.Scope, runID uint64,
	reason model.ReasonCode, _ time.Time) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.waitingRuns[runID] {
		return false, nil
	}
	w.waitingRuns[runID] = false
	w.closedRuns[runID] = reason
	return true, nil
}

func (w *receiptWorld) InTx(_ context.Context, fn func(*repository.DB) error) error {
	w.mu.Lock()
	w.txCalls++
	s := w.snap()
	w.mu.Unlock()
	err := fn(nil)
	if err != nil {
		w.mu.Lock()
		w.restore(s)
		w.mu.Unlock()
	}
	return err
}

func sameFarm(ref model.OrderRef, tenant, farm uint64) bool {
	return ref.TenantID == tenant && ref.FarmID == farm
}

func (w *receiptWorld) OrderForDispatch(_ context.Context, _ *repository.DB, ref model.OrderRef) (repository.DispatchOrder, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.orders[ref.OrderID]
	if !ok || !sameFarm(ref, o.TenantID, o.FarmID) {
		return repository.DispatchOrder{}, fmt.Errorf("order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	return o, nil
}

func (w *receiptWorld) AttemptTx(_ context.Context, _ *repository.DB, ref model.OrderRef, id uint64) (model.DispatchAttempt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.attempts[id]
	if !ok || !sameFarm(ref, a.TenantID, a.FarmID) || a.OrderID != ref.OrderID {
		return model.DispatchAttempt{}, fmt.Errorf("attempt %d: %w", id, model.ErrNotFound)
	}
	return a, nil
}

func (w *receiptWorld) LatestAttemptTx(_ context.Context, _ *repository.DB, ref model.OrderRef) (model.DispatchAttempt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var best model.DispatchAttempt
	for _, a := range w.attempts {
		if sameFarm(ref, a.TenantID, a.FarmID) && a.OrderID == ref.OrderID && a.Attempt > best.Attempt {
			best = a
		}
	}
	if best.ID == 0 {
		return best, fmt.Errorf("attempts of order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	return best, nil
}

func (w *receiptWorld) InsertReceiptTx(_ context.Context, _ *repository.DB, r repository.ReceiptRow) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	k := fmt.Sprintf("%d:%d:%s", r.TenantID, r.FarmID, r.ReceiptID)
	if _, ok := w.receipts[k]; ok {
		return false, nil
	}
	w.receipts[k] = r
	return true, nil
}

func (w *receiptWorld) UpdateReceiptTx(_ context.Context, _ *repository.DB, ref model.OrderRef, id uint64, rowVer int,
	status model.AttemptStatus, kind model.FailureKind, area *float64, due, now time.Time) error {
	if err := model.CheckAttemptState(status, kind, area); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.attempts[id]
	if !ok || !sameFarm(ref, a.TenantID, a.FarmID) || a.RowVer != rowVer {
		return model.ErrConcurrentUpdate
	}
	a.Status, a.FailureKind, a.ReceiptDueAt, a.UpdatedAt = status, kind, due, now
	if area != nil {
		v := *area
		a.DoneAreaMu = &v
	}
	a.RowVer++
	w.attempts[id] = a
	return nil
}

func (w *receiptWorld) MarkNoReceiptTx(_ context.Context, _ *repository.DB, ref model.OrderRef, id uint64, rowVer int, now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.attempts[id]
	if !ok || !sameFarm(ref, a.TenantID, a.FarmID) || a.RowVer != rowVer || !inFlight(a.Status) {
		return model.ErrConcurrentUpdate
	}
	a.Status, a.FailureKind, a.UpdatedAt = model.AttemptNoReceipt, model.FailureUnknown, now
	a.RowVer++
	w.attempts[id] = a
	return nil
}

func (w *receiptWorld) SetDoneAreaTx(_ context.Context, _ *repository.DB, ref model.OrderRef, area float64, _ time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	o := w.orders[ref.OrderID]
	if s := model.WorkOrderStatus(o.Status); s != model.OrderDone && s != model.OrderPartiallyDone {
		return model.ErrConcurrentUpdate
	}
	w.doneArea[ref.OrderID] = area
	return nil
}

func (w *receiptWorld) InsertWorkDoneMemoryTx(_ context.Context, _ *repository.DB, ref model.OrderRef, _ uint64, summary string, _ time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.memories[ref.OrderID]; !ok {
		w.memories[ref.OrderID] = summary
	}
	return nil
}

func (w *receiptWorld) insertOutbox(topic, dedup string, payload []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.outbox {
		if m.Dedup == dedup {
			return false
		}
	}
	w.outbox = append(w.outbox, rwOutbox{Topic: topic, Dedup: dedup, Payload: payload})
	return true
}

func (w *receiptWorld) InsertFieldHistoryOutboxTx(_ context.Context, _ *repository.DB, ref model.OrderRef, ticketID uint64, payload []byte, _ time.Time) error {
	w.insertOutbox(repository.TopicFieldHistoryAppend, fmt.Sprintf("field.history.append:%d:%d:1", ref.OrderID, ticketID), payload)
	return nil
}

func (w *receiptWorld) InsertRetryDispatchOutboxTx(_ context.Context, _ *repository.DB, ref model.OrderRef, ticketID uint64, attempt int,
	payload []byte, _ time.Time) error {
	if attempt < 2 {
		return fmt.Errorf("retry attempt %d < 2", attempt)
	}
	w.insertOutbox(repository.TopicOrderDispatch, fmt.Sprintf("order.dispatch:%d:%d:%d", ref.OrderID, ticketID, attempt), payload)
	return nil
}

func (w *receiptWorld) InsertCancelOutboxTx(_ context.Context, _ *repository.DB, ref model.OrderRef, ticketID uint64, payload []byte, _ time.Time) (bool, error) {
	return w.insertOutbox(repository.TopicOrderCancel, fmt.Sprintf("order.cancel:%d:%d:1", ref.OrderID, ticketID), payload), nil
}

func (w *receiptWorld) PendingReconcile(_ context.Context, scope model.Scope, limit int) ([]model.DispatchAttempt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []model.DispatchAttempt
	for _, a := range w.attempts {
		if a.TenantID == scope.TenantID && a.FarmID == scope.FarmID && model.AwaitingReconcile(a.Status, a.FailureKind) && len(out) < limit {
			out = append(out, a)
		}
	}
	return out, nil
}

func (w *receiptWorld) ByJob(_ context.Context, _ repository.SystemScope, deviceType, jobID string) (repository.AttemptOwner, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lookupErr != nil {
		return repository.AttemptOwner{}, w.lookupErr
	}
	for _, a := range w.attempts {
		if a.DeviceType == deviceType && a.DeviceJobID == jobID {
			return repository.AttemptOwner{ID: a.ID, TenantID: a.TenantID, FarmID: a.FarmID, OrderID: a.OrderID,
				Attempt: a.Attempt, Status: a.Status}, nil
		}
	}
	return repository.AttemptOwner{}, model.ErrNotFound
}

func (w *receiptWorld) DueAttempts(_ context.Context, _ repository.SystemScope, now time.Time) ([]repository.DueAttempt, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []repository.DueAttempt
	for _, a := range w.attempts {
		if inFlight(a.Status) && a.ReceiptDueAt.Before(now) {
			out = append(out, repository.DueAttempt{ID: a.ID, TenantID: a.TenantID, FarmID: a.FarmID, OrderID: a.OrderID})
		}
	}
	slices.SortFunc(out, func(x, y repository.DueAttempt) int { return int(x.ID) - int(y.ID) })
	return out, nil
}

func (w *receiptWorld) Transition(ctx context.Context, tx *repository.DB, ref model.OrderRef, to model.WorkOrderStatus,
	actor model.Actor, reason model.ReasonCode) error {
	w.mu.Lock()
	from := model.WorkOrderStatus(w.orders[ref.OrderID].Status)
	w.mu.Unlock()
	return w.TransitionFrom(ctx, tx, ref, from, to, actor, reason)
}

func (w *receiptWorld) TransitionFrom(_ context.Context, _ *repository.DB, ref model.OrderRef, from, to model.WorkOrderStatus,
	actor model.Actor, reason model.ReasonCode) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failTrans != nil {
		return w.failTrans
	}
	o, ok := w.orders[ref.OrderID]
	if !ok || model.WorkOrderStatus(o.Status) != from {
		return model.ErrConcurrentUpdate
	}
	if !model.CanTransition(from, to) {
		return fmt.Errorf("%s -> %s: %w", from, to, model.ErrIllegalTransition)
	}
	o.Status = string(to)
	w.orders[ref.OrderID] = o
	w.transitions = append(w.transitions, rwTransition{OrderID: ref.OrderID, From: from, To: to, Actor: actor, Reason: reason})
	return nil
}

func (w *receiptWorld) order(id uint64) repository.DispatchOrder {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.orders[id]
}

func (w *receiptWorld) attempt(id uint64) model.DispatchAttempt {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attempts[id]
}

func (w *receiptWorld) topics(topic string) []rwOutbox {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []rwOutbox
	for _, m := range w.outbox {
		if m.Topic == topic {
			out = append(out, m)
		}
	}
	return out
}

// scriptDevices 是 ReceiptDevices 的脚本实现：按调用顺序出队，记调用次数。
type scriptDevices struct {
	mu      sync.Mutex
	queries []devResult
	cancels []devResult
	queryN  int
	cancelN int
}

type devResult struct {
	r   model.DeviceReceipt
	err error
}

func (d *scriptDevices) QueryStatus(context.Context, DeviceTarget, string, string) (model.DeviceReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queryN++
	return pop(&d.queries)
}

func (d *scriptDevices) Cancel(context.Context, DeviceTarget, string) (model.DeviceReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancelN++
	return pop(&d.cancels)
}

func pop(q *[]devResult) (model.DeviceReceipt, error) {
	if len(*q) == 0 {
		return model.DeviceReceipt{}, fmt.Errorf("script exhausted")
	}
	r := (*q)[0]
	*q = (*q)[1:]
	return r.r, r.err
}

// memDenier 记录跨农场拒绝。
type memDenier struct{ denials []AccessDenial }

func (m *memDenier) Deny(_ context.Context, d AccessDenial) { m.denials = append(m.denials, d) }

// memFarmTenants 返回固定的农场租户。
type memFarmTenants map[uint64][]uint64

func (m memFarmTenants) FarmTenants(_ context.Context, farmID uint64) ([]uint64, error) {
	return m[farmID], nil
}
