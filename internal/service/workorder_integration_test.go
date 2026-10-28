//go:build integration

package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

func TestTransition_Integration(t *testing.T) {
	e := itest.Setup(t, "svc")
	tests := []struct {
		name    string
		from    string
		to      model.WorkOrderStatus
		stale   bool // 事务内先把 row_ver 改掉，模拟并发更新
		wantErr error
		wantRow int64 // 期望 transition / run_event 行数
	}{
		{name: "draft to pending", from: "DRAFT", to: model.OrderPendingApproval, wantRow: 1},
		{name: "illegal draft to done", from: "DRAFT", to: model.OrderDone, wantErr: model.ErrIllegalTransition},
		{name: "terminal done", from: "DONE", to: model.OrderCanceled, wantErr: model.ErrIllegalTransition},
		{name: "concurrent row_ver", from: "DRAFT", to: model.OrderPendingApproval, stale: true,
			wantErr: model.ErrConcurrentUpdate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot := e.Fresh(t, t0)
			runID := itest.InsertRun(t, slot.DB, t0)
			orderID := itest.InsertOrder(t, slot.DB, runID, tt.from, t0)
			ref := model.OrderRef{TenantID: itest.TenantID, FarmID: itest.FarmID, OrderID: orderID}
			svc := service.NewWorkOrderService(slot.Clock, staleStore{stale: tt.stale}, repository.EventRepo{})
			slot.Clock.Advance(time.Minute)

			err := slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
				return svc.Transition(t.Context(), tx, ref, tt.to, model.Actor{Type: model.ActorUser, ID: 1001}, "")
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			trans := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM work_order_transition WHERE order_id = ?", orderID)
			events := itest.Count(t, slot.DB,
				"SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = 'ORDER_TRANSITION' AND seq = 1", runID)
			seq := itest.Count(t, slot.DB, "SELECT event_seq FROM agent_run WHERE id = ?", runID)
			if trans != tt.wantRow || events != tt.wantRow || seq != tt.wantRow {
				t.Fatalf("transition=%d run_event=%d event_seq=%d, want %d (failed tx must roll back)",
					trans, events, seq, tt.wantRow)
			}
			if tt.wantErr == nil {
				st := itest.Count(t, slot.DB,
					"SELECT COUNT(*) FROM work_order WHERE id = ? AND status = ? AND row_ver = 1 AND updated_at = ?",
					orderID, string(tt.to), slot.Clock.Now())
				if st != 1 {
					t.Fatal("work_order not updated to new status / row_ver / frozen updated_at")
				}
			}
		})
	}
}

// staleStore 包装真实 WorkOrderRepo；stale 时把读到的 row_ver 减一，让定版乐观锁 UPDATE 命中 0 行。
type staleStore struct {
	repository.WorkOrderRepo
	stale bool
}

func (s staleStore) GetState(ctx context.Context, tx *repository.DB, ref model.OrderRef) (model.OrderState, error) {
	st, err := s.WorkOrderRepo.GetState(ctx, tx, ref)
	if s.stale {
		st.RowVer--
	}
	return st, err
}
