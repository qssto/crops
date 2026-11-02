package sim_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/adapter/devicesim"
	"agri-steward/internal/adapter/devicesim/sim"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	d, err := sim.LoadDevices(filepath.Join("..", "..", "..", "..", "configs", "mock", "device.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := sim.New(fixedClock{t0}, d)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s %s: decode: %v", method, url, err)
	}
	return resp.StatusCode, raw
}

func TestScripts_HTTP(t *testing.T) {
	ts := newServer(t)
	cases := []struct {
		name, path, body string
		want             int
	}{
		{"ok", "/sim/v1/scripts/5", `{"steps":[{"state":"running"},{"state":"failed","fail_stage":"in_progress"}]}`, 204},
		{"no receipt", "/sim/v1/scripts/6", `{"steps":[]}`, 204},
		{"bad order id", "/sim/v1/scripts/x", `{"steps":[]}`, 400},
		{"unknown field", "/sim/v1/scripts/5", `{"stepz":[]}`, 400},
		{"unknown submit", "/sim/v1/scripts/5", `{"submit":"maybe","steps":[]}`, 400},
		{"accepted as step", "/sim/v1/scripts/5", `{"steps":[{"state":"accepted"}]}`, 400},
		{"failed without stage", "/sim/v1/scripts/5", `{"steps":[{"state":"failed"}]}`, 400},
		{"stage without failed", "/sim/v1/scripts/5", `{"steps":[{"state":"finished","fail_stage":"unknown"}]}`, 400},
		{"negative delay", "/sim/v1/scripts/5", `{"delay_ms":-1,"steps":[]}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := do(t, http.MethodPut, ts.URL+tc.path, tc.body); code != tc.want {
				t.Fatalf("status = %d (%s), want %d", code, body, tc.want)
			}
		})
	}
	bad := map[string]string{
		"missing idem key": `{"tenant_id":1,"farm_id":1,"order_id":5}`,
		"missing order":    `{"idem_key":"k","tenant_id":1,"farm_id":1}`,
		"not json":         `{`,
	}
	for name, body := range bad {
		if code, _ := do(t, http.MethodPost, ts.URL+"/v1/jobs", body); code != http.StatusBadRequest {
			t.Fatalf("submit %s = %d, want 400", name, code)
		}
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/v1/jobs", ""); code != http.StatusBadRequest {
		t.Fatalf("query without external_no = %d", code)
	}
}

func TestDevices_HTTP(t *testing.T) {
	ts := newServer(t)
	code, raw := do(t, http.MethodGet, ts.URL+"/v1/devices?tenant_id=1&farm_id=2", "")
	var list devicesim.DeviceList
	if err := json.Unmarshal(raw, &list); err != nil || code != 200 {
		t.Fatalf("list = %d %s, %v", code, raw, err)
	}
	if list.Source != sim.SourceName || len(list.Devices) != 1 || list.Devices[0].DeviceID != "uav-p100-21" {
		t.Fatalf("farm 2 devices = %+v", list)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/v1/devices?tenant_id=1", ""); code != http.StatusBadRequest {
		t.Fatalf("tenant without farm = %d, want 400", code)
	}
	code, raw = do(t, http.MethodGet, ts.URL+"/v1/devices/uav-p100-01", "")
	var dv devicesim.DeviceInfo
	if err := json.Unmarshal(raw, &dv); err != nil || code != 200 {
		t.Fatalf("get = %d %s, %v", code, raw, err)
	}
	if !dv.Status.Online || dv.Status.BatteryPct == nil || *dv.Status.BatteryPct != 92 ||
		!dv.Status.LastSeenAt.Equal(t0.Add(-30*time.Second)) || dv.FarmID != 1 || len(dv.Capabilities) != 2 {
		t.Fatalf("device = %+v", dv)
	}
	if code, _ := do(t, http.MethodGet, ts.URL+"/v1/devices/uav-404", ""); code != http.StatusNotFound {
		t.Fatalf("unknown device = %d", code)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := sim.New(nil, sim.DeviceFile{}); err == nil {
		t.Fatal("nil clock must be rejected")
	}
	dup := sim.DeviceFile{Devices: []sim.DeviceSpec{{DeviceID: "a", TenantID: 1, FarmID: 1}, {DeviceID: "a", TenantID: 1, FarmID: 1}}}
	if _, err := sim.New(fixedClock{t0}, dup); err == nil {
		t.Fatal("duplicate device must be rejected")
	}
	if _, err := sim.New(fixedClock{t0}, sim.DeviceFile{Devices: []sim.DeviceSpec{{DeviceID: "a"}}}); err == nil {
		t.Fatal("unbound device must be rejected")
	}
}
