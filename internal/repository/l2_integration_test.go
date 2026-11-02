//go:build integration

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/adapter/devicesim"
	"agri-steward/internal/adapter/devicesim/sim"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func newAttempt(orderID uint64, attempt int, idem string) model.DispatchAttempt {
	return model.DispatchAttempt{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID, TicketID: 5,
		Attempt: attempt, DeviceType: "uav", DeviceID: "uav-p100-01", IdemKey: idem, Mode: model.DispatchReal,
		CommandRawID: 41, FlagReadAt: t0, SentAt: t0, ReceiptDueAt: t0.Add(10 * time.Minute)}
}

// TestDispatchAttempt_Integration：P1-L2 第 5 项。SENT 插入、唯一键冲突（uk_order_attempt / uk_idem / uk_job）、
// row_ver 乐观锁冲突、按 Scope 的待对账列表、只经 SystemScope 的 uk_job 反查。
func TestDispatchAttempt_Integration(t *testing.T) {
	e := itest.Setup(t, "repol2")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.DispatchAttemptRepo{DB: slot.DB}
	tx := repository.Transactor{DB: slot.DB}
	orderID := itest.InsertOrder(t, slot.DB, itest.InsertRun(t, slot.DB, t0), "APPROVED", t0)
	ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID}
	insert := func(a model.DispatchAttempt) (uint64, error) {
		var id uint64
		err := tx.InTx(ctx, func(tx *repository.DB) error {
			var err error
			id, err = repo.InsertSentTx(ctx, tx, a)
			return err
		})
		return id, err
	}
	idem := fmt.Sprintf("order.dispatch:%d:5:1", orderID)
	id, err := insert(newAttempt(orderID, 1, idem))
	if err != nil || id == 0 {
		t.Fatalf("insert = %d, %v", id, err)
	}
	if _, err := insert(newAttempt(orderID, 1, idem+"-x")); !errors.Is(err, repository.ErrDuplicateAttempt) {
		t.Fatalf("same order+attempt err = %v, want ErrDuplicateAttempt (uk_order_attempt)", err)
	}
	if _, err := insert(newAttempt(orderID, 2, idem)); !errors.Is(err, repository.ErrDuplicateAttempt) {
		t.Fatalf("same idem_key err = %v, want ErrDuplicateAttempt (uk_idem)", err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM dispatch_attempt WHERE order_id=?", orderID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	accepted := model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "sim-1"}
	setJob := func(rowVer int) error {
		return tx.InTx(ctx, func(tx *repository.DB) error {
			return repo.SetJobTx(ctx, tx, ref, id, rowVer, accepted, t0.Add(20*time.Minute), t0.Add(time.Second))
		})
	}
	if err := setJob(0); err != nil {
		t.Fatal(err)
	}
	if err := setJob(0); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("stale row_ver set job err = %v, want ErrConcurrentUpdate", err)
	}
	updateReceipt := func(r model.OrderRef, rowVer int, st model.AttemptStatus, kind model.FailureKind) error {
		return tx.InTx(ctx, func(tx *repository.DB) error {
			return repo.UpdateReceiptTx(ctx, tx, r, id, rowVer, st, kind, nil, t0.Add(30*time.Minute), t0.Add(2*time.Second))
		})
	}
	if err := updateReceipt(ref, 0, model.AttemptExecuting, ""); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("stale row_ver receipt err = %v, want ErrConcurrentUpdate", err)
	}
	other := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID + 1, OrderID: orderID}
	if err := updateReceipt(other, 1, model.AttemptExecuting, ""); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("other farm receipt err = %v, want ErrConcurrentUpdate (scoped WHERE)", err)
	}
	if err := updateReceipt(ref, 1, model.AttemptFailed, model.FailureUnknown); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ByOrder(ctx, ref)
	if err != nil || len(got) != 1 {
		t.Fatalf("ByOrder = %+v, %v", got, err)
	}
	a := got[0]
	if a.ID != id || a.Status != model.AttemptFailed || a.FailureKind != model.FailureUnknown || a.DeviceJobID != "sim-1" ||
		a.RowVer != 2 || a.DoneAreaMu != nil || a.Mode != model.DispatchReal || a.IdemKey != idem ||
		!a.SentAt.Equal(t0) || !a.ReceiptDueAt.Equal(t0.Add(30*time.Minute)) || !a.UpdatedAt.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("attempt = %+v", a)
	}

	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	pending, err := repo.PendingReconcile(ctx, scope, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("PendingReconcile = %+v, %v", pending, err)
	}
	if pending, err := repo.PendingReconcile(ctx, model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}, 10); err != nil ||
		len(pending) != 0 {
		t.Fatalf("other farm pending = %+v, %v", pending, err)
	}

	sys := repository.NewSystemScope("l2_receipt_lookup")
	owner, err := repo.ByJob(ctx, sys, "uav", "sim-1")
	if err != nil || owner.ID != id || owner.TenantID != itest.TenantID || owner.FarmID != itest.FarmID ||
		owner.OrderID != orderID || owner.Attempt != 1 || owner.Status != model.AttemptFailed {
		t.Fatalf("ByJob = %+v, %v", owner, err)
	}
	if _, err := repo.ByJob(ctx, sys, "valve", "sim-1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ByJob other type err = %v, want ErrNotFound", err)
	}

	// uk_job：同一 device_type 下平台任务号不得指向两次尝试。
	id2, err := insert(newAttempt(orderID, 2, idem+"-2"))
	if err != nil {
		t.Fatal(err)
	}
	err = tx.InTx(ctx, func(tx *repository.DB) error {
		return repo.SetJobTx(ctx, tx, ref, id2, 0, accepted, t0.Add(20*time.Minute), t0.Add(time.Second))
	})
	if !errors.Is(err, repository.ErrDuplicateAttempt) {
		t.Fatalf("same job id err = %v, want ErrDuplicateAttempt (uk_job)", err)
	}

	// 审查修复：不合规状态不写库；done_area_mu 传 nil 保留旧值（COALESCE）。
	write := func(rowVer int, st model.AttemptStatus, kind model.FailureKind, area *float64) error {
		return tx.InTx(ctx, func(tx *repository.DB) error {
			return repo.UpdateReceiptTx(ctx, tx, ref, id, rowVer, st, kind, area, t0.Add(40*time.Minute), t0.Add(3*time.Second))
		})
	}
	if err := write(2, model.AttemptFailed, "", nil); !errors.Is(err, model.ErrBadReceipt) {
		t.Fatalf("FAILED without kind err = %v, want ErrBadReceipt", err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM dispatch_attempt WHERE id=? AND row_ver=2 AND failure_kind='UNKNOWN'", id); n != 1 {
		t.Fatal("rejected receipt must not touch the row")
	}
	area := 4.5
	if err := write(2, model.AttemptPartiallyDone, "", &area); err != nil {
		t.Fatal(err)
	}
	if err := write(3, model.AttemptCanceled, "", nil); err != nil {
		t.Fatal(err)
	}
	got, err = repo.ByOrder(ctx, ref)
	if err != nil || len(got) != 2 || got[0].Status != model.AttemptCanceled || got[0].DoneAreaMu == nil ||
		*got[0].DoneAreaMu != area || got[0].RowVer != 4 {
		t.Fatalf("after nil area update = %+v, %v; want done_area_mu kept at %v", got, err, area)
	}
}

// memObjects 是带读回的内存对象存储（代替 MinIO）。
type memObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memObjects) PutObject(_ context.Context, bucket, key string, body []byte, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objs == nil {
		m.objs = map[string][]byte{}
	}
	m.objs[bucket+"/"+key] = append([]byte(nil), body...)
	return nil
}

func (m *memObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("object %s/%s: %w", bucket, key, model.ErrNotFound)
	}
	return b, nil
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type traceSink struct{ traces []model.DeviceCallTrace }

func (s *traceSink) RecordDeviceCall(_ context.Context, tr model.DeviceCallTrace) {
	s.traces = append(s.traces, tr)
}

// TestTracedCommandReadBack_Integration：P1-L2 第 6 项。Traced 包装 devicesim 客户端下发，原始指令经真实 RawStore
// 落 raw_object（kind=DEVICE_CMD），按 run_id 查到后经 RawReader 读回，与 trace 的 command_raw_id 一致。
func TestTracedCommandReadBack_Integration(t *testing.T) {
	e := itest.Setup(t, "repol2")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	runID := itest.InsertRun(t, slot.DB, t0)
	orderID := itest.InsertOrder(t, slot.DB, runID, "APPROVED", t0)

	srv, err := sim.New(fixedClock{t0}, sim.DeviceFile{})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client, err := devicesim.New(devicesim.Config{BaseURL: ts.URL, Timeout: 2 * time.Second,
		Caps: adapter.Capabilities{Cancel: true, IdemKey: true, ExternalQuery: true}})
	if err != nil {
		t.Fatal(err)
	}
	objs := &memObjects{}
	bucket := e.Cfg.MinIO.BucketRaw
	sink := &traceSink{}
	tr, err := adapter.NewTraced("uav", client, sink, repository.NewRawStore(slot.DB, objs, bucket), fixedClock{t0})
	if err != nil {
		t.Fatal(err)
	}
	order := model.WorkOrder{OrderID: orderID, RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID, FieldID: 1,
		DeviceType: "uav", WorkType: model.WorkSpray, Payload: []byte(`{"area_mu":5}`),
		TimeWindow: model.TimeWindow{From: t0, To: t0.Add(time.Hour)}}
	idem := fmt.Sprintf("order.dispatch:%d:5:1", orderID)
	r, err := tr.Dispatch(ctx, order, idem)
	if err != nil || r.Status != model.AttemptAccepted || r.DeviceJobID == "" {
		t.Fatalf("Dispatch = %+v, %v", r, err)
	}

	var rawIDs []uint64
	if err := slot.DB.WithContext(ctx).Raw(
		`SELECT id FROM raw_object WHERE tenant_id=? AND farm_id=? AND run_id=? AND kind=?`,
		itest.TenantID, itest.FarmID, runID, string(model.RawDeviceCmd)).Scan(&rawIDs).Error; err != nil {
		t.Fatal(err)
	}
	if len(rawIDs) != 1 || len(sink.traces) != 1 || sink.traces[0].CommandRawID != rawIDs[0] {
		t.Fatalf("raw ids = %v, traces = %+v", rawIDs, sink.traces)
	}
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	body, err := repository.RawReader{DB: slot.DB, Obj: objs, Bucket: bucket}.Get(ctx, scope, rawIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"idem_key":"`+idem+`"`) || !strings.Contains(string(body), fmt.Sprintf(`"order_id":%d`, orderID)) {
		t.Fatalf("command body = %s", body)
	}
	if _, err := (repository.RawReader{DB: slot.DB, Obj: objs, Bucket: bucket}).Get(ctx,
		model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}, rawIDs[0]); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other farm read err = %v, want ErrNotFound", err)
	}
	// 重试同一 idem_key：不新增 raw_object（uk_key 幂等）。
	if _, err := tr.Dispatch(ctx, order, idem); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM raw_object WHERE run_id=? AND kind='DEVICE_CMD'", runID); n != 1 {
		t.Fatalf("raw rows after retry = %d, want 1", n)
	}
}
