package servers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/internal/adapter/devicesim/sim"
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
)

// startDevicesim 起本地设备模拟平台（与 mock 同一份 configs/mock/device.yaml、同一时钟）。
func startDevicesim(t *testing.T) string {
	t.Helper()
	d, err := sim.LoadDevices(filepath.Join(repoRoot, "configs", "mock", "device.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.New(fixedClock{testNow}, d)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// validateOutput 校验 data 通过 srv 上 tool 的 OutputSchema。
func validateOutput(t *testing.T, srv mcp.Server, tool string, data json.RawMessage) {
	t.Helper()
	for _, sp := range srv.Tools() {
		if sp.Name != tool {
			continue
		}
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		if err := compile(t, tool, sp.OutputSchema).Validate(doc); err != nil {
			t.Fatalf("output %s does not match schema: %v", data, err)
		}
		return
	}
	t.Fatalf("no tool %s", tool)
}

func realDeviceConfig(base string) *config.Tools {
	cfg := testConfig(config.ToolModeMock)
	cfg.Device.Mode = config.ToolModeReal
	cfg.Device.Real.BaseURL = base
	return cfg
}

// deviceView 是调用方关心的 device.status 字段（L3 续航载药 checker 只读这些）。
type deviceView struct {
	DeviceID   string   `json:"device_id"`
	Online     bool     `json:"online"`
	State      string   `json:"state"`
	BatteryPct *float64 `json:"battery_pct"`
	TankL      *float64 `json:"tank_l"`
	LastSeenAt string   `json:"last_seen_at"`
}

// deviceCaller 是「同一调用方代码」：经 Build 装配后按工具名调用，不感知 mock / real。
func deviceCaller(t *testing.T, cfg *config.Tools) (list []string, st deviceView, fresh int, source string) {
	t.Helper()
	srvs, err := Build(cfg, fixedClock{testNow}, farmFields{1: testFields})
	if err != nil {
		t.Fatal(err)
	}
	var dev mcp.Server
	for _, s := range srvs {
		if s.Name() == "device" {
			dev = s
		}
	}
	ctx := mcp.WithScope(context.Background(), farm1Scope)
	res, err := dev.Invoke(ctx, "device.list", json.RawMessage(`{"field_id":3,"kind":"drone"}`))
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
	for _, d := range out.Devices {
		list = append(list, d.DeviceID)
	}
	res, err = dev.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"uav-p100-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(res.Data, &st); err != nil {
		t.Fatal(err)
	}
	return list, st, res.DataFreshnessSec, res.Source
}

// TestDevice_MockAndRealSameCaller：P1-L2 第 7 项。mock / real 只由配置切换，同一调用方代码在两种配置下结果一致
// （在线 / 续航 / 载药 / 心跳与新鲜度），source 分别为 mock 数据源与 devicesim。
func TestDevice_MockAndRealSameCaller(t *testing.T) {
	base := startDevicesim(t)
	mList, mSt, mFresh, mSrc := deviceCaller(t, testConfig(config.ToolModeMock))
	rList, rSt, rFresh, rSrc := deviceCaller(t, realDeviceConfig(base))
	if !slices.Equal(mList, []string{"uav-p100-01"}) || !slices.Equal(rList, mList) {
		t.Fatalf("device.list mock=%v real=%v", mList, rList)
	}
	if mSt.DeviceID != rSt.DeviceID || mSt.Online != rSt.Online || mSt.State != rSt.State || mSt.LastSeenAt != rSt.LastSeenAt ||
		mSt.BatteryPct == nil || rSt.BatteryPct == nil || *mSt.BatteryPct != *rSt.BatteryPct ||
		mSt.TankL == nil || rSt.TankL == nil || *mSt.TankL != *rSt.TankL {
		t.Fatalf("device.status mock=%+v real=%+v", mSt, rSt)
	}
	if mFresh != 30 || rFresh != mFresh {
		t.Fatalf("data_freshness_sec mock=%d real=%d, want 30", mFresh, rFresh)
	}
	if mSrc != "mock-xgeo" || rSrc != sim.SourceName {
		t.Fatalf("source mock=%q real=%q", mSrc, rSrc)
	}
}

// TestDeviceReal_StatusSchemaAndErrors：real 输出过 device.status 的 output schema（含新增 capabilities），
// 他场 / 不存在设备 SCOPE_DENIED，平台不可用 / 非 200 / 非 JSON 分别归一为 UPSTREAM / UPSTREAM / MALFORMED。
func TestDeviceReal_StatusSchemaAndErrors(t *testing.T) {
	base := startDevicesim(t)
	srvs, err := Build(realDeviceConfig(base), fixedClock{testNow}, farmFields{1: testFields})
	if err != nil {
		t.Fatal(err)
	}
	var dev mcp.Server
	for _, s := range srvs {
		if s.Name() == "device" {
			dev = s
		}
	}
	ctx := mcp.WithScope(context.Background(), farm1Scope)
	res, err := dev.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"uav-p100-01"}`))
	if err != nil {
		t.Fatal(err)
	}
	validateOutput(t, dev, "device.status", res.Data)
	var st struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(res.Data, &st); err != nil || !slices.Equal(st.Capabilities, []string{"spray", "spread"}) {
		t.Fatalf("capabilities = %v, %v", st.Capabilities, err)
	}
	for _, id := range []string{"uav-p100-21", "uav-404"} {
		_, err := dev.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"`+id+`"}`))
		var te *mcp.ToolError
		if !errors.As(err, &te) || te.Code != mcp.ErrScopeDenied {
			t.Fatalf("%s err = %v, want SCOPE_DENIED", id, err)
		}
	}
	bad := map[string]struct {
		h    http.HandlerFunc
		want mcp.ErrorCode
	}{
		"5xx":      {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }, mcp.ErrUpstream},
		"not json": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, mcp.ErrMalformed},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(tc.h)
			defer ts.Close()
			srvs, err := Build(realDeviceConfig(ts.URL), fixedClock{testNow}, farmFields{1: testFields})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range srvs {
				if s.Name() != "device" {
					continue
				}
				_, err := s.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"uav-p100-01"}`))
				var te *mcp.ToolError
				if !errors.As(err, &te) || te.Code != tc.want {
					t.Fatalf("err = %v, want %s", err, tc.want)
				}
			}
		})
	}
	bogus := realDeviceConfig("ftp://x")
	if _, err := Build(bogus, fixedClock{testNow}, testFields); err == nil {
		t.Fatal("bad real base_url must fail Build")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	cfg := realDeviceConfig(slow.URL)
	cfg.Device.Timeout = 50 * time.Millisecond
	srvs, err = Build(cfg, fixedClock{testNow}, farmFields{1: testFields})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srvs {
		if s.Name() == "device" {
			_, err := s.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"uav-p100-01"}`))
			var te *mcp.ToolError
			if !errors.As(err, &te) || te.Code != mcp.ErrTimeout {
				t.Fatalf("slow platform err = %v, want TIMEOUT", err)
			}
		}
	}
}

// TestDeviceOwnership_Real：real 配了 base_url 时按设备平台的本农场清单判归属（与 mock 绑定一致）。
func TestDeviceOwnership_Real(t *testing.T) {
	cfg := realDeviceConfig(startDevicesim(t))
	cfg.Device.Timeout = 2 * time.Second
	b, err := DeviceOwnership(cfg)
	if err != nil {
		t.Fatal(err)
	}
	farm2 := model.Scope{TenantID: 1, FarmID: 2}
	for _, tt := range []struct {
		scope model.Scope
		id    string
		want  bool
	}{
		{farm1Scope, "uav-p100-01", true},
		{farm1Scope, "uav-p100-21", false},
		{farm2, "uav-p100-21", true},
		{model.Scope{TenantID: 2, FarmID: 1}, "uav-p100-01", false},
		{model.Scope{}, "uav-p100-01", false},
	} {
		if ok, err := b.OwnsDevice(context.Background(), tt.scope, tt.id); err != nil || ok != tt.want {
			t.Fatalf("OwnsDevice(%+v, %s) = %v, %v; want %v", tt.scope, tt.id, ok, err, tt.want)
		}
	}
	ids, err := b.DeviceIDs(context.Background(), farm2)
	if err != nil || !slices.Equal(ids, []string{"uav-p100-21"}) {
		t.Fatalf("farm2 ids = %v, %v", ids, err)
	}
	down, err := DeviceOwnership(realDeviceConfig("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := down.OwnsDevice(context.Background(), farm1Scope, "uav-p100-01"); err == nil || ok {
		t.Fatalf("platform down: OwnsDevice = %v, %v; want error", ok, err)
	}
}

// fakePlatform 起一个忽略 tenant_id / farm_id 过滤、原样返回 body 的平台。
func fakePlatform(t *testing.T, body string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

const platformDev = `{"device_id":%q,"tenant_id":%d,"farm_id":%d,"kind":"drone","model":"P100","capabilities":["spray"],
"fields":["field-003"],"status":{"online":true,"state":"idle","battery_pct":90,"last_seen_at":%q}}`

func platformBody(devs ...string) string {
	return `{"source":"devicesim","updated_at":"2026-08-20T02:00:00Z","devices":[` + strings.Join(devs, ",") + `]}`
}

func platformDevice(id string, tenant, farm uint64, lastSeen string) string {
	return fmt.Sprintf(platformDev, id, tenant, farm, lastSeen)
}

// TestDeviceReal_PlatformLeaksOtherFarm：平台忽略过滤、返回他场设备 → 四处入口一律失败关闭（UPSTREAM），
// OwnsDevice 为 false 且报错，list 不返回他场设备，status 不返回数据。
func TestDeviceReal_PlatformLeaksOtherFarm(t *testing.T) {
	const seen = "2026-08-20T01:59:30Z"
	base := fakePlatform(t, platformBody(
		platformDevice("uav-p100-01", 1, 1, seen), platformDevice("uav-p100-21", 1, 2, seen)))
	cfg := realDeviceConfig(base)
	b, err := DeviceOwnership(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"uav-p100-01", "uav-p100-21"} {
		if ok, err := b.OwnsDevice(context.Background(), farm1Scope, id); ok || err == nil {
			t.Fatalf("OwnsDevice(%s) = %v, %v; want false + error", id, ok, err)
		}
	}
	if ids, err := b.DeviceIDs(context.Background(), farm1Scope); err == nil || len(ids) != 0 {
		t.Fatalf("DeviceIDs = %v, %v; want error", ids, err)
	}
	dev := deviceServer(t, cfg)
	ctx := mcp.WithScope(context.Background(), farm1Scope)
	for _, call := range []struct{ tool, args string }{
		{"device.list", `{"field_id":3}`},
		{"device.status", `{"device_id":"uav-p100-01"}`},
		{"device.status", `{"device_id":"uav-p100-21"}`},
	} {
		res, err := dev.Invoke(ctx, call.tool, json.RawMessage(call.args))
		var te *mcp.ToolError
		if !errors.As(err, &te) || te.Code != mcp.ErrUpstream || res.Data != nil {
			t.Fatalf("%s %s = %s, %v; want UPSTREAM and no data", call.tool, call.args, res.Data, err)
		}
	}
}

// TestDeviceReal_MissingCollectedAt：平台给零值 last_seen_at / updated_at → MALFORMED（采集时间缺失），不透传 0001 年。
func TestDeviceReal_MissingCollectedAt(t *testing.T) {
	zero := "0001-01-01T00:00:00Z"
	dev := deviceServer(t, realDeviceConfig(fakePlatform(t, platformBody(platformDevice("uav-p100-01", 1, 1, zero)))))
	ctx := mcp.WithScope(context.Background(), farm1Scope)
	_, err := dev.Invoke(ctx, "device.status", json.RawMessage(`{"device_id":"uav-p100-01"}`))
	var te *mcp.ToolError
	if !errors.As(err, &te) || te.Code != mcp.ErrMalformed || !strings.Contains(te.Msg, "last_seen_at") {
		t.Fatalf("status err = %v, want MALFORMED missing last_seen_at", err)
	}
	noUpdated := `{"source":"devicesim","devices":[` + platformDevice("uav-p100-01", 1, 1, "2026-08-20T01:59:30Z") + `]}`
	dev = deviceServer(t, realDeviceConfig(fakePlatform(t, noUpdated)))
	if _, err := dev.Invoke(ctx, "device.list", json.RawMessage(`{"field_id":3}`)); !errors.As(err, &te) || te.Code != mcp.ErrMalformed {
		t.Fatalf("list err = %v, want MALFORMED missing updated_at", err)
	}
}

func deviceServer(t *testing.T, cfg *config.Tools) mcp.Server {
	t.Helper()
	srvs, err := Build(cfg, fixedClock{testNow}, farmFields{1: testFields})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srvs {
		if s.Name() == "device" {
			return s
		}
	}
	t.Fatal("no device server")
	return nil
}
