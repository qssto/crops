package servers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
)

// farmFields 是按农场分组的 FieldDirectory：farm_id → 本农场地块。
type farmFields map[uint64]kit.StaticFields

func (f farmFields) FieldCode(ctx context.Context, s model.Scope, id uint64) (string, error) {
	return f[s.FarmID].FieldCode(ctx, s, id)
}

// TestInvoke_FarmScoped：Server 按 ctx 注入的 Scope 解析地块与设备；他场地块 EMPTY，
// 设备按 device.yaml 显式绑定判归属，他场 / 不存在 / 无 scope 同为 SCOPE_DENIED；device.list 只列本农场设备。
func TestInvoke_FarmScoped(t *testing.T) {
	dir := farmFields{
		1: {1: "field-001", 2: "field-002", 3: "field-003", 4: "field-004", 5: "field-005", 6: "field-006"},
		2: {201: "field-201", 202: "field-202"}, // 示范农场2（configs/seed/fields.yaml）
	}
	srvs, err := Build(testConfig(config.ToolModeMock), fixedClock{testNow}, dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]mcp.Server{}
	for _, s := range srvs {
		byName[s.Name()] = s
	}
	farm1 := mcp.WithScope(context.Background(), model.Scope{TenantID: 1, FarmID: 1})
	farm2 := mcp.WithScope(context.Background(), model.Scope{TenantID: 1, FarmID: 2})
	otherTenant := mcp.WithScope(context.Background(), model.Scope{TenantID: 2, FarmID: 1}) // 同号农场、他租户
	tests := []struct {
		name, server, tool, args string
		ctx                      context.Context
		want                     mcp.ErrorCode // 空表示成功
	}{
		{"own field", "field", "field.profile", `{"field_id":1}`, farm1, ""},
		{"other farm field", "field", "field.profile", `{"field_id":201}`, farm1, mcp.ErrEmpty},
		{"own device", "device", "device.status", `{"device_id":"uav-p100-01"}`, farm1, ""},
		{"farm2 own device", "device", "device.status", `{"device_id":"uav-p100-21"}`, farm2, ""},
		{"other farm device", "device", "device.status", `{"device_id":"uav-p100-21"}`, farm1, mcp.ErrScopeDenied},
		{"farm1 device from farm2", "device", "device.status", `{"device_id":"pump-01"}`, farm2, mcp.ErrScopeDenied},
		{"other tenant same farm_id", "device", "device.status", `{"device_id":"uav-p100-01"}`, otherTenant, mcp.ErrScopeDenied},
		{"unknown device", "device", "device.status", `{"device_id":"uav-404"}`, farm1, mcp.ErrScopeDenied},
		{"no scope", "device", "device.status", `{"device_id":"uav-p100-01"}`, context.Background(), mcp.ErrScopeDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := byName[tt.server].Invoke(tt.ctx, tt.tool, json.RawMessage(tt.args))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want ok", err)
				}
				return
			}
			var te *mcp.ToolError
			if !errors.As(err, &te) || te.Code != tt.want {
				t.Fatalf("err = %v, want code %s", err, tt.want)
			}
		})
	}
}

// TestDeviceList_OnlyOwnFarm：device.list 先按显式绑定限定本农场设备，再按地块编码匹配。
func TestDeviceList_OnlyOwnFarm(t *testing.T) {
	dir := farmFields{1: {3: "field-003"}, 2: {201: "field-201"}}
	srvs, err := Build(testConfig(config.ToolModeMock), fixedClock{testNow}, dir)
	if err != nil {
		t.Fatal(err)
	}
	var dev mcp.Server
	for _, s := range srvs {
		if s.Name() == "device" {
			dev = s
		}
	}
	tests := []struct {
		name  string
		scope model.Scope
		args  string
		want  []string
	}{
		{"farm1 field-003", model.Scope{TenantID: 1, FarmID: 1}, `{"field_id":3}`, []string{"uav-p100-01", "pump-01", "sensor-soil-03"}},
		{"farm2 field-201", model.Scope{TenantID: 1, FarmID: 2}, `{"field_id":201}`, []string{"uav-p100-21"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := dev.Invoke(mcp.WithScope(context.Background(), tt.scope), "device.list", json.RawMessage(tt.args))
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Devices []struct {
					DeviceID string `json:"device_id"`
				} `json:"devices"`
			}
			if err := json.Unmarshal(res.Data, &out); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(out.Devices))
			for _, d := range out.Devices {
				got = append(got, d.DeviceID)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("devices = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDeviceOwnership：mock 读 device.yaml 显式绑定；real 为空绑定（全部拒绝，真实绑定待 P1-L2）。
func TestDeviceOwnership(t *testing.T) {
	farm1, farm2 := model.Scope{TenantID: 1, FarmID: 1}, model.Scope{TenantID: 1, FarmID: 2}
	mock, err := DeviceOwnership(testConfig(config.ToolModeMock))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := mock.DeviceIDs(context.Background(), farm2)
	if err != nil || !slices.Equal(ids, []string{"uav-p100-21"}) {
		t.Fatalf("farm2 devices = %v, %v", ids, err)
	}
	tests := []struct {
		name  string
		scope model.Scope
		id    string
		want  bool
	}{
		{"own", farm1, "uav-p100-01", true},
		{"other farm", farm1, "uav-p100-21", false},
		{"unknown", farm1, "uav-404", false},
		{"zero scope", model.Scope{}, "uav-p100-01", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ok, err := mock.OwnsDevice(context.Background(), tt.scope, tt.id); err != nil || ok != tt.want {
				t.Fatalf("OwnsDevice = %v, %v; want %v", ok, err, tt.want)
			}
		})
	}
	denyAll, err := DeviceOwnership(testConfig(config.ToolModeReal))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := denyAll.OwnsDevice(context.Background(), farm1, "uav-p100-01"); err != nil || ok {
		t.Fatalf("real OwnsDevice = %v, %v; want deny-all", ok, err)
	}
	if _, err := DeviceOwnership(&config.Tools{Device: config.ToolServer{Mode: "bogus"}}); err == nil {
		t.Fatal("unknown mode must fail")
	}
}
