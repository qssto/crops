package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

type fakeRechecker struct {
	res agent.RecheckResult
	err error
	got agent.RecheckRequest
}

func (f *fakeRechecker) Recheck(_ context.Context, req agent.RecheckRequest) (agent.RecheckResult, error) {
	f.got = req
	return f.res, f.err
}

type noTx struct{}

func (noTx) InTx(_ context.Context, fn func(tx *repository.DB) error) error { return fn(nil) }

type fakeStarter struct {
	inputs []AskInput
	err    error
}

func (f *fakeStarter) Ask(_ context.Context, _ model.Scope, in AskInput) (uint64, error) {
	f.inputs = append(f.inputs, in)
	return 501, f.err
}

func TestRecheckService_PreDispatch(t *testing.T) {
	q := agent.Question{Text: "3号田的稻飞虱要不要打药？", Scope: model.Scope{TenantID: 1, FarmID: 1, UserID: initiator},
		FieldID: 3, RunID: 77}
	keys, err := rediskey.New("test")
	if err != nil {
		t.Fatal(err)
	}
	in := PreDispatchInput{Order: model.OrderRef{TenantID: 1, FarmID: 1, OrderID: 9}, Question: q, Epoch: 2}
	tests := []struct {
		name       string
		rc         fakeRechecker
		lockHeld   bool
		starterErr error
		wantErr    error
		wantTo     []model.WorkOrderStatus
		wantRun    uint64
	}{
		{name: "unchanged keeps order", rc: fakeRechecker{}},
		{name: "changed cancels and reasks", rc: fakeRechecker{res: agent.RecheckResult{Changed: true, Detail: "rain"}},
			wantTo: []model.WorkOrderStatus{model.OrderCanceled}, wantRun: 501},
		{name: "inconclusive leaves order", rc: fakeRechecker{err: agent.ErrRecheckInconclusive},
			wantErr: agent.ErrRecheckInconclusive},
		{name: "order busy", rc: fakeRechecker{res: agent.RecheckResult{Changed: true}}, lockHeld: true, wantErr: ErrOrderBusy},
		{name: "new run failure surfaces after cancel", rc: fakeRechecker{res: agent.RecheckResult{Changed: true}},
			starterErr: ErrRateLimited, wantErr: ErrRateLimited, wantTo: []model.WorkOrderStatus{model.OrderCanceled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, orders, starter := tt.rc, &memTransitions{}, &fakeStarter{err: tt.starterErr}
			svc := NewRecheckService(RecheckDeps{Rechecker: &rc, Tx: noTx{}, Orders: orders, Starter: starter,
				Locks: memLocks{held: tt.lockHeld}, Keys: keys, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			got, err := svc.PreDispatch(context.Background(), in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if rc.got.Epoch != in.Epoch || rc.got.Question.RunID != q.RunID {
				t.Errorf("recheck request = %+v", rc.got)
			}
			if !reflect.DeepEqual(orders.to, tt.wantTo) {
				t.Errorf("transitions = %v, want %v", orders.to, tt.wantTo)
			}
			if tt.wantErr == nil && got.NewRunID != tt.wantRun {
				t.Errorf("new run = %d, want %d", got.NewRunID, tt.wantRun)
			}
			if tt.wantRun != 0 && (len(starter.inputs) != 1 || starter.inputs[0] != (AskInput{FieldID: 3, Text: q.Text})) {
				t.Errorf("reask inputs = %+v", starter.inputs)
			}
		})
	}
}
