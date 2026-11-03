package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// BindDeviceTx 写 payload.device_id（row_ver 守卫）；dropBind 时假装成功不落库。
func (m *memDispatch) BindDeviceTx(_ context.Context, _ *repository.DB, _ model.OrderRef, deviceID string, rowVer int,
	_ time.Time) error {
	if err := m.fail("bind"); err != nil {
		return err
	}
	if rowVer != m.order.RowVer {
		return model.ErrConcurrentUpdate
	}
	if m.dropBind {
		return nil
	}
	p := map[string]any{}
	if len(m.order.Payload) > 0 {
		if err := json.Unmarshal(m.order.Payload, &p); err != nil {
			return err
		}
	}
	p["device_id"] = deviceID
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	m.order.Payload, m.order.RowVer = b, m.order.RowVer+1
	return nil
}

// InsertSentTx 同 uk_idem / uk_order_attempt 冲突 → ErrDuplicateAttempt。
func (m *memDispatch) InsertSentTx(_ context.Context, _ *repository.DB, a model.DispatchAttempt) (uint64, error) {
	if err := m.fail("sent"); err != nil {
		return 0, err
	}
	for _, x := range m.attempts {
		if x.IdemKey == a.IdemKey || (x.OrderID == a.OrderID && x.Attempt == a.Attempt) {
			return 0, repository.ErrDuplicateAttempt
		}
	}
	a.ID, a.Status, a.RowVer = uint64(len(m.attempts)+1), model.AttemptSent, 0
	m.attempts = append(m.attempts, a)
	return a.ID, nil
}

func (m *memDispatch) attempt(id uint64, rowVer int) (*model.DispatchAttempt, error) {
	for i := range m.attempts {
		if m.attempts[i].ID == id {
			if m.attempts[i].RowVer != rowVer {
				return nil, model.ErrConcurrentUpdate
			}
			return &m.attempts[i], nil
		}
	}
	return nil, model.ErrConcurrentUpdate
}

func (m *memDispatch) SetJobTx(_ context.Context, _ *repository.DB, _ model.OrderRef, id uint64, rowVer int,
	rc model.DeviceReceipt, receiptDueAt, _ time.Time) error {
	if err := m.fail("job"); err != nil {
		return err
	}
	if rc.DeviceJobID == "" {
		return fmt.Errorf("empty device_job_id")
	}
	if err := rc.Validate(); err != nil {
		return err
	}
	a, err := m.attempt(id, rowVer)
	if err != nil {
		return err
	}
	a.DeviceJobID, a.Status, a.FailureKind, a.ReceiptDueAt, a.RowVer = rc.DeviceJobID, rc.Status, rc.FailureKind,
		receiptDueAt, a.RowVer+1
	return nil
}

func (m *memDispatch) UpdateReceiptTx(_ context.Context, _ *repository.DB, _ model.OrderRef, id uint64, rowVer int,
	status model.AttemptStatus, kind model.FailureKind, _ *float64, receiptDueAt, _ time.Time) error {
	if status == model.AttemptSent {
		return model.ErrBadReceipt
	}
	if err := model.CheckAttemptState(status, kind, nil); err != nil {
		return err
	}
	a, err := m.attempt(id, rowVer)
	if err != nil {
		return err
	}
	a.Status, a.FailureKind, a.ReceiptDueAt, a.RowVer = status, kind, receiptDueAt, a.RowVer+1
	return nil
}

func (m *memDispatch) RefreshSentTx(_ context.Context, _ *repository.DB, _ model.OrderRef, id uint64, rowVer int,
	flagReadAt, sentAt, receiptDueAt time.Time) error {
	if err := m.fail("refresh"); err != nil {
		return err
	}
	a, err := m.attempt(id, rowVer)
	if err != nil {
		return err
	}
	if a.Status != model.AttemptSent || a.DeviceJobID != "" {
		return model.ErrConcurrentUpdate
	}
	a.FlagReadAt, a.SentAt, a.ReceiptDueAt, a.RowVer = flagReadAt, sentAt, receiptDueAt, a.RowVer+1
	return nil
}

func (m *memDispatch) ByIdemKey(_ context.Context, _ *repository.DB, _ model.Scope, idemKey string) (model.DispatchAttempt, error) {
	for _, a := range m.attempts {
		if a.IdemKey == idemKey {
			return a, nil
		}
	}
	return model.DispatchAttempt{}, fmt.Errorf("attempt %s: %w", idemKey, model.ErrNotFound)
}

// fakePicker 是 DevicePicker 的桩。
type fakePicker struct {
	id    string
	err   error
	calls int
}

func (p *fakePicker) PickDevice(context.Context, model.WorkOrder, int) (string, error) {
	p.calls++
	return p.id, p.err
}

// fakeDevice 是 Traced 装饰后的真实 Adapter 桩（DeviceAdapter + CommandArchiver + ArchivedDispatcher），记录调用次数。
// block 为 true 时 DispatchArchived 阻塞到 ctx 取消（模拟续期失败中断设备 IO）。
type fakeDevice struct {
	mu         sync.Mutex
	caps       adapter.Capabilities
	rc         model.DeviceReceipt
	err        error
	queryRC    model.DeviceReceipt
	queryErr   error
	block      bool
	archives   int
	dispatches int
	queries    int
	keys       []string
	rawIDs     []uint64
}

func (f *fakeDevice) Capabilities() adapter.Capabilities { return f.caps }

func (f *fakeDevice) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, fmt.Errorf("untraced Dispatch must not be called")
}

func (f *fakeDevice) QueryStatus(context.Context, adapter.StatusRef) (model.DeviceReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	return f.queryRC, f.queryErr
}

func (f *fakeDevice) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, adapter.ErrCapability
}

func (f *fakeDevice) Archive(context.Context, model.WorkOrder, string) (model.RawObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archives++
	return model.RawObject{ID: 600}, nil
}

func (f *fakeDevice) DispatchArchived(ctx context.Context, _ model.WorkOrder, idemKey string,
	rawID uint64) (model.DeviceReceipt, error) {
	f.mu.Lock()
	f.dispatches++
	f.keys, f.rawIDs = append(f.keys, idemKey), append(f.rawIDs, rawID)
	block, rc, err := f.block, f.rc, f.err
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return model.DeviceReceipt{}, context.Cause(ctx)
	}
	return rc, err
}

// fakeRegistry 按 device_type 取 Adapter，未登记 → ErrDeviceNotOpen。
type fakeRegistry map[string]adapter.DeviceAdapter

func (r fakeRegistry) Get(t string) (adapter.DeviceAdapter, error) {
	if a, ok := r[t]; ok {
		return a, nil
	}
	return nil, adapter.ErrDeviceNotOpen
}

// fakeReopener 模拟 ReopenService：Reopen 把单退回 PENDING_APPROVAL。
type fakeReopener struct {
	store   *memDispatch
	err     error
	got     ReopenRequest
	prepare int
	reopen  int
}

func (f *fakeReopener) Prepare(_ context.Context, req ReopenRequest) (ReopenPlan, error) {
	f.prepare++
	f.got = req
	return ReopenPlan{}, f.err
}

func (f *fakeReopener) Reopen(context.Context, ReopenPlan) (model.ApprovalTicket, error) {
	f.reopen++
	f.store.order.Status = string(model.OrderPendingApproval)
	return model.ApprovalTicket{TicketID: 88}, nil
}
