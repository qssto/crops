package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
)

func simDevices(caps config.DeviceCapabilities) *config.Devices {
	return &config.Devices{Devicesim: config.Devicesim{Enabled: true, BaseURL: "http://127.0.0.1:8090",
		Timeout: time.Second, DeviceTypes: []string{adapter.DeviceTypeUAV}, Capabilities: caps}}
}

// TestBuild_Devices：P1 §6.3 验收 2。默认四类骨架全部不可开放；devicesim 开启时 uav 可开放；
// 能力不满足开放规则或参数非法时 Build 失败（启动自检）。
func TestBuild_Devices(t *testing.T) {
	all := config.DeviceCapabilities{Cancel: true, IdemKey: true, ExternalQuery: true}
	remote := simDevices(all)
	remote.Devicesim.BaseURL = "http://devices.example.com:8090"
	tests := []struct {
		name     string
		env      string
		devices  *config.Devices
		uavOpen  bool
		wantErr  error
		buildErr bool
	}{
		{name: "default skeletons", devices: nil},
		{name: "devicesim disabled", devices: &config.Devices{}},
		{name: "devicesim disabled in prod", env: config.EnvProd, devices: &config.Devices{}},
		{name: "devicesim uav", env: "dev", devices: simDevices(all), uavOpen: true},
		{name: "devicesim staging", env: "staging", devices: simDevices(all), uavOpen: true},
		{name: "no cancel", env: "dev", devices: simDevices(config.DeviceCapabilities{IdemKey: true}), wantErr: adapter.ErrCapability, buildErr: true},
		{name: "cancel only", env: "dev", devices: simDevices(config.DeviceCapabilities{Cancel: true}), wantErr: adapter.ErrCapability, buildErr: true},
		{name: "bad base url", env: "dev", devices: &config.Devices{Devicesim: config.Devicesim{Enabled: true, BaseURL: "ftp://x",
			DeviceTypes: []string{"uav"}, Capabilities: all}}, wantErr: config.ErrDevicesimNotAllowed, buildErr: true},
		// 生产保护：env=prod 开启、env 未给、base_url 非回环都使 Build 失败。
		{name: "devicesim in prod", env: config.EnvProd, devices: simDevices(all), wantErr: config.ErrDevicesimNotAllowed, buildErr: true},
		{name: "devicesim without env", devices: simDevices(all), wantErr: config.ErrDevicesimNotAllowed, buildErr: true},
		{name: "non-loopback base url", env: "dev", devices: remote, wantErr: config.ErrDevicesimNotAllowed, buildErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fullDeps(t)
			d.Devices = tt.devices
			d.Env = tt.env
			a, err := Build(d)
			if tt.buildErr {
				if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
					t.Fatalf("err = %v, want build failure %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Devices.Openable(adapter.DeviceTypeUAV) != tt.uavOpen {
				t.Fatalf("uav openable = %v, want %v", a.Devices.Openable(adapter.DeviceTypeUAV), tt.uavOpen)
			}
			for _, typ := range []string{adapter.DeviceTypeVehicle, adapter.DeviceTypeValve, adapter.DeviceTypeFertigation} {
				if a.Devices.Openable(typ) || !a.Devices.Has(typ) {
					t.Fatalf("%s must be a closed skeleton", typ)
				}
			}
		})
	}
}

type fakeFlags struct {
	mode model.DispatchMode
	err  error
}

func (f fakeFlags) DispatchMode(context.Context, model.Scope) (model.DispatchMode, error) {
	return f.mode, f.err
}

// TestDeviceDispatchMode：to_order 只有「开关开 ∧ 设备类型可开放」才写 REAL（P1-L2 第 3 / 9 项）。
func TestDeviceDispatchMode(t *testing.T) {
	errFlag := errors.New("flag store down")
	open := func(typ string) bool { return typ == adapter.DeviceTypeUAV }
	tests := []struct {
		name    string
		flags   fakeFlags
		device  string
		want    model.DispatchMode
		wantErr error
	}{
		{"flag on + openable", fakeFlags{mode: model.DispatchReal}, "uav", model.DispatchReal, nil},
		{"flag on + skeleton", fakeFlags{mode: model.DispatchReal}, "valve", model.DispatchDryRun, nil},
		{"flag on + unregistered", fakeFlags{mode: model.DispatchReal}, "DRONE", model.DispatchDryRun, nil},
		{"flag off + openable", fakeFlags{mode: model.DispatchDryRun}, "uav", model.DispatchDryRun, nil},
		{"flag error", fakeFlags{mode: model.DispatchReal, err: errFlag}, "uav", model.DispatchDryRun, errFlag},
	}
	scope := model.Scope{TenantID: 1, FarmID: 1}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deviceDispatchMode{flags: tt.flags, open: open}.DispatchModeFor(context.Background(), scope, tt.device)
			if got != tt.want || !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("DispatchModeFor = %s, %v; want %s, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
