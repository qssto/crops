package adapter_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

// stubDevice 是只声明能力的 DeviceAdapter（Registry 只看 Capabilities）。
type stubDevice struct{ caps adapter.Capabilities }

func (s stubDevice) Capabilities() adapter.Capabilities { return s.caps }
func (stubDevice) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "j"}, nil
}
func (stubDevice) QueryStatus(context.Context, adapter.StatusRef) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, nil
}
func (stubDevice) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, nil
}

// TestRegister_OpenRule：开放规则（必须 Cancel；IdemKey 与 ExternalQuery 至少一项）逐项校验，违反即注册失败。
func TestRegister_OpenRule(t *testing.T) {
	cases := []struct {
		name string
		caps adapter.Capabilities
		ok   bool
	}{
		{"all", adapter.Capabilities{Cancel: true, IdemKey: true, ExternalQuery: true}, true},
		{"cancel + idem", adapter.Capabilities{Cancel: true, IdemKey: true}, true},
		{"cancel + external", adapter.Capabilities{Cancel: true, ExternalQuery: true}, true},
		{"no cancel", adapter.Capabilities{IdemKey: true, ExternalQuery: true}, false},
		{"cancel only", adapter.Capabilities{Cancel: true}, false},
		{"none", adapter.Capabilities{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := adapter.NewRegistry()
			err := r.Register("uav", stubDevice{tc.caps})
			if tc.ok != (err == nil) {
				t.Fatalf("Register err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && (!errors.Is(err, adapter.ErrRegistry) || !errors.Is(err, adapter.ErrCapability)) {
				t.Fatalf("err = %v, want ErrRegistry wrapping ErrCapability", err)
			}
			if r.Openable("uav") != tc.ok || r.Has("uav") != tc.ok {
				t.Fatalf("Openable/Has = %v/%v after err %v", r.Openable("uav"), r.Has("uav"), err)
			}
		})
	}
}

// ptrDevice 是指针接收者的 Adapter，用于构造 typed nil。
type ptrDevice struct{ caps adapter.Capabilities }

func (p *ptrDevice) Capabilities() adapter.Capabilities { return p.caps }
func (p *ptrDevice) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, nil
}
func (p *ptrDevice) QueryStatus(context.Context, adapter.StatusRef) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, nil
}
func (p *ptrDevice) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, nil
}

func TestRegistry_Entries(t *testing.T) {
	ok := stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}
	cases := []struct {
		name string
		do   func(r *adapter.Registry) error
	}{
		{"empty type", func(r *adapter.Registry) error { return r.Register("", ok) }},
		{"nil adapter", func(r *adapter.Registry) error { return r.Register("uav", nil) }},
		{"typed nil adapter", func(r *adapter.Registry) error {
			var p *ptrDevice // Capabilities() 解引用会 panic：须在注册入口拦下
			return r.Register("uav", p)
		}},
		{"closed without reason", func(r *adapter.Registry) error { return r.RegisterClosed("uav", "") }},
		{"open twice", func(r *adapter.Registry) error {
			if err := r.Register("uav", ok); err != nil {
				return nil
			}
			return r.Register("uav", ok)
		}},
		{"open then closed", func(r *adapter.Registry) error {
			if err := r.Register("uav", ok); err != nil {
				return nil
			}
			return r.RegisterClosed("uav", "x")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(adapter.NewRegistry()); !errors.Is(err, adapter.ErrRegistry) {
				t.Fatalf("err = %v, want ErrRegistry", err)
			}
		})
	}
}

// TestRegisterSkeletons：四类真实设备骨架登记为不可开放，Get / 调用返回 ErrDeviceNotOpen；已登记类型（预发 uav → devicesim）跳过。
func TestRegisterSkeletons(t *testing.T) {
	ctx := context.Background()
	r := adapter.NewRegistry()
	if err := r.Register(adapter.DeviceTypeUAV, stubDevice{adapter.Capabilities{Cancel: true, IdemKey: true}}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.RegisterSkeletons(r); err != nil {
		t.Fatal(err)
	}
	if !r.Openable(adapter.DeviceTypeUAV) {
		t.Fatal("pre-registered uav must stay openable")
	}
	for _, typ := range []string{adapter.DeviceTypeVehicle, adapter.DeviceTypeValve, adapter.DeviceTypeFertigation} {
		if r.Openable(typ) || !r.Has(typ) {
			t.Fatalf("%s: Openable=%v Has=%v, want closed skeleton", typ, r.Openable(typ), r.Has(typ))
		}
		if _, err := r.Get(typ); !errors.Is(err, adapter.ErrDeviceNotOpen) {
			t.Fatalf("Get(%s) err = %v", typ, err)
		}
	}
	for _, typ := range []string{"knapsack", "manual", ""} {
		if r.Openable(typ) {
			t.Fatalf("unregistered %q must not be openable", typ)
		}
		if _, err := r.Get(typ); !errors.Is(err, adapter.ErrDeviceNotOpen) {
			t.Fatalf("Get(%q) err = %v", typ, err)
		}
	}
	entries := r.Entries()
	var types []string
	for _, e := range entries {
		types = append(types, e.DeviceType)
	}
	if !slices.Equal(types, []string{"fertigation", "uav", "valve", "vehicle"}) {
		t.Fatalf("entry types = %v, want sorted four", types)
	}
	for _, e := range entries {
		if e.Open != (e.DeviceType == adapter.DeviceTypeUAV) || (!e.Open && e.Reason == "") {
			t.Fatalf("entry %+v", e)
		}
	}
	for _, s := range adapter.Skeletons() {
		if err := adapter.CheckCapabilities(s.Capabilities()); err == nil {
			t.Fatalf("skeleton %s must fail the open rule", s.DeviceType)
		}
		if _, err := s.Dispatch(ctx, model.WorkOrder{}, "k"); !errors.Is(err, adapter.ErrDeviceNotOpen) {
			t.Fatalf("skeleton Dispatch err = %v", err)
		}
		if _, err := s.QueryStatus(ctx, adapter.StatusRef{IdemKey: "k"}); !errors.Is(err, adapter.ErrDeviceNotOpen) {
			t.Fatalf("skeleton QueryStatus err = %v", err)
		}
		if _, err := s.Cancel(ctx, "j"); !errors.Is(err, adapter.ErrDeviceNotOpen) {
			t.Fatalf("skeleton Cancel err = %v", err)
		}
	}
	all := adapter.NewRegistry()
	if err := adapter.RegisterSkeletons(all); err != nil || all.Openable(adapter.DeviceTypeUAV) {
		t.Fatalf("default registry: err=%v uav openable=%v, want all closed", err, all.Openable(adapter.DeviceTypeUAV))
	}
}
