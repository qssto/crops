package graph_test

import (
	"context"
	"errors"
	"testing"

	"agri-steward/internal/agent/graph"
	"agri-steward/internal/model"
)

// fakeDispatchMode 是 agent.DeviceDispatchModeSource 的桩：记录读取次数、Scope 与设备类型。
type fakeDispatchMode struct {
	mode   model.DispatchMode
	err    error
	calls  int
	scope  model.Scope
	device string
}

func (f *fakeDispatchMode) DispatchModeFor(_ context.Context, scope model.Scope, deviceType string) (model.DispatchMode, error) {
	f.calls++
	f.scope = scope
	f.device = deviceType
	return f.mode, f.err
}

// to_order 读一次开关写定 dispatch_mode：开 → REAL，关 → DRY_RUN，读失败与未装配都按 DRY_RUN。
func TestAgentAsk_DispatchMode(t *testing.T) {
	h := newHarness(t)
	s := findSample(t, loadSamples(t), sOperation)
	tests := []struct {
		name      string
		src       *fakeDispatchMode
		want      model.DispatchMode
		wantCalls int
	}{
		{name: "flag on writes REAL", src: &fakeDispatchMode{mode: model.DispatchReal}, want: model.DispatchReal, wantCalls: 1},
		{name: "flag off writes DRY_RUN", src: &fakeDispatchMode{mode: model.DispatchDryRun}, want: model.DispatchDryRun,
			wantCalls: 1},
		{name: "read error fails closed", src: &fakeDispatchMode{mode: model.DispatchReal, err: errors.New("mysql down")},
			want: model.DispatchDryRun, wantCalls: 1},
		{name: "not wired keeps DRY_RUN", want: model.DispatchDryRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemStore()
			clk := sampleClock(t, s)
			a := h.newAgent(t, clk, deps{tools: fixtureTools(t, s, clk), store: store, cfg: func(c *graph.Config) {
				if tt.src != nil {
					c.DispatchMode = tt.src
				}
			}})
			q, err := questionOf(s)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := a.Ask(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if ans.Order == nil {
				t.Fatal("no order")
			}
			if got := store.orders[ans.Card.AdviceID].DispatchMode; got != tt.want || ans.Order.DispatchMode != tt.want {
				t.Errorf("dispatch_mode stored %s / answer %s, want %s", got, ans.Order.DispatchMode, tt.want)
			}
			if tt.src == nil {
				return
			}
			if tt.src.calls != tt.wantCalls || tt.src.scope.FarmID != q.Scope.FarmID || tt.src.scope.TenantID != q.Scope.TenantID {
				t.Errorf("source calls=%d scope=%+v, want %d / %+v", tt.src.calls, tt.src.scope, tt.wantCalls, q.Scope)
			}
			// P1-L2：按作业单的 device_type 判定（Registry.Openable 在 app 侧组合）。
			if tt.src.device == "" || tt.src.device != ans.Order.DeviceType {
				t.Errorf("source device_type = %q, want order device_type %q", tt.src.device, ans.Order.DeviceType)
			}
		})
	}
}
