package adapter_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

func TestMockAdapterDryRun(t *testing.T) {
	win := model.TimeWindow{From: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)}
	payload := json.RawMessage(`{"product":"吡虫啉","dose_per_mu":{"value":10,"unit":"g"},"total":{"value":300,"unit":"g"},"field_id":3,"area_mu":30}`)
	tests := []struct {
		name    string
		order   model.WorkOrder
		wantOps []string
		wantErr error
	}{
		{name: "spray", order: model.WorkOrder{OrderID: 1, FieldID: 3, WorkType: model.WorkSpray, DeviceType: "drone",
			Payload: payload, TimeWindow: win}, wantOps: []string{adapter.OpLoad, adapter.OpSpray, adapter.OpReturn}},
		{name: "fertilize", order: model.WorkOrder{OrderID: 2, FieldID: 3, WorkType: model.WorkFertilize, DeviceType: "manual",
			Payload: payload, TimeWindow: win}, wantOps: []string{adapter.OpLoad, adapter.OpSpread, adapter.OpReturn}},
		{name: "irrigate", order: model.WorkOrder{OrderID: 3, FieldID: 3, WorkType: model.WorkIrrigate, DeviceType: "valve",
			Payload: json.RawMessage(`{"total":{"value":20,"unit":"m3"}}`), TimeWindow: win},
			wantOps: []string{adapter.OpValveOpen, adapter.OpValveClose}},
		{name: "unknown work type", order: model.WorkOrder{OrderID: 4, WorkType: "HARVEST", Payload: payload},
			wantErr: adapter.ErrUnsupportedWork},
		{name: "bad payload", order: model.WorkOrder{OrderID: 5, WorkType: model.WorkSpray, Payload: json.RawMessage(`[1]`)},
			wantErr: errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := adapter.MockAdapter{}.DryRun(context.Background(), tt.order)
			if tt.wantErr != nil {
				if err == nil || (tt.wantErr != errAny && !errors.Is(err, tt.wantErr)) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var cmds []adapter.Command
			if err := json.Unmarshal(res.Commands, &cmds); err != nil {
				t.Fatalf("commands %s: %v", res.Commands, err)
			}
			if len(cmds) != len(tt.wantOps) {
				t.Fatalf("commands = %s, want ops %v", res.Commands, tt.wantOps)
			}
			for i, c := range cmds {
				if c.Op != tt.wantOps[i] || c.Seq != i+1 || c.FieldID != tt.order.FieldID || c.DeviceType != tt.order.DeviceType {
					t.Errorf("command %d = %+v", i, c)
				}
			}
		})
	}
}

// errAny 表示只要求返回错误。
var errAny = errors.New("any error")

func TestMockAdapterDispatchDisabled(t *testing.T) {
	m := adapter.MockAdapter{}
	if _, err := m.Dispatch(context.Background(), model.WorkOrder{}, "k"); !errors.Is(err, adapter.ErrRealDispatchDisabled) {
		t.Fatalf("Dispatch err = %v", err)
	}
	if m.Name() != adapter.MockName {
		t.Fatalf("name = %q", m.Name())
	}
}

func TestMockAdapterCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (adapter.MockAdapter{}).DryRun(ctx, model.WorkOrder{WorkType: model.WorkSpray}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
