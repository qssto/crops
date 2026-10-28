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

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type fakeOrders struct {
	state       model.OrderState
	getErr      error
	updateErr   error
	updated     bool
	transitions []model.OrderTransition
}

func (f *fakeOrders) GetState(context.Context, *repository.DB, model.OrderRef) (model.OrderState, error) {
	return f.state, f.getErr
}

func (f *fakeOrders) UpdateStatus(_ context.Context, _ *repository.DB, _ model.OrderRef, from, _ model.WorkOrderStatus, rowVer int, _ time.Time) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if from != f.state.Status || rowVer != f.state.RowVer {
		return model.ErrConcurrentUpdate
	}
	f.updated = true
	return nil
}

func (f *fakeOrders) InsertTransition(_ context.Context, _ *repository.DB, t model.OrderTransition) error {
	f.transitions = append(f.transitions, t)
	return nil
}

type fakeEvents struct{ events []model.RunEvent }

func (f *fakeEvents) AppendTx(_ context.Context, _ *repository.DB, e model.RunEvent) (int64, error) {
	f.events = append(f.events, e)
	return int64(len(f.events)), nil
}

func TestTransition(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	ref := model.OrderRef{TenantID: 1, FarmID: 1, OrderID: 9}
	actor := model.Actor{Type: model.ActorUser, ID: 1002}
	tests := []struct {
		name      string
		orders    *fakeOrders
		to        model.WorkOrderStatus
		wantErr   error
		wantWrite bool
	}{
		{name: "draft to pending", to: model.OrderPendingApproval, wantWrite: true,
			orders: &fakeOrders{state: model.OrderState{RunID: 5, Status: model.OrderDraft, RowVer: 3}}},
		{name: "illegal draft to done", to: model.OrderDone, wantErr: model.ErrIllegalTransition,
			orders: &fakeOrders{state: model.OrderState{RunID: 5, Status: model.OrderDraft}}},
		{name: "terminal state", to: model.OrderPendingApproval, wantErr: model.ErrIllegalTransition,
			orders: &fakeOrders{state: model.OrderState{RunID: 5, Status: model.OrderCanceled}}},
		{name: "row_ver conflict", to: model.OrderApproved, wantErr: model.ErrConcurrentUpdate,
			orders: &fakeOrders{state: model.OrderState{RunID: 5, Status: model.OrderPendingApproval},
				updateErr: model.ErrConcurrentUpdate}},
		{name: "not found", to: model.OrderApproved, wantErr: model.ErrNotFound,
			orders: &fakeOrders{getErr: model.ErrNotFound}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := &fakeEvents{}
			svc := NewWorkOrderService(fixedClock{now}, tt.orders, events)
			err := svc.Transition(context.Background(), nil, ref, tt.to, actor, model.ReasonApprovalRejected)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !tt.wantWrite {
				if len(tt.orders.transitions) != 0 || len(events.events) != 0 {
					t.Fatalf("failed transition must not write: %+v %+v", tt.orders.transitions, events.events)
				}
				return
			}
			if len(tt.orders.transitions) != 1 || len(events.events) != 1 {
				t.Fatalf("writes = %d transitions, %d events", len(tt.orders.transitions), len(events.events))
			}
			tr, ev := tt.orders.transitions[0], events.events[0]
			if tr.From != model.OrderDraft || tr.To != tt.to || tr.RunID != 5 || !tr.CreatedAt.Equal(now) {
				t.Errorf("transition = %+v", tr)
			}
			if ev.Type != model.EventOrderTransition || ev.RunID != 5 || ev.Actor != model.ActorUser {
				t.Errorf("event = %+v", ev)
			}
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil || p["to"] != string(tt.to) {
				t.Errorf("payload = %s (%v)", ev.Payload, err)
			}
		})
	}
}
