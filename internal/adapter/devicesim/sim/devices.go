package sim

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/adapter/devicesim"
)

// DeviceFile 是设备清单文件（格式同 configs/mock/device.yaml，模拟平台与 mcp-device mock 共用一份数据）。
type DeviceFile struct {
	Source    string       `yaml:"source"`
	UpdatedAt time.Time    `yaml:"updated_at"`
	Devices   []DeviceSpec `yaml:"devices"`
}

// DeviceSpec 是一台设备；Status.HeartbeatLag 是最近心跳距当前时刻的时长。
type DeviceSpec struct {
	DeviceID     string     `yaml:"device_id"`
	TenantID     uint64     `yaml:"tenant_id"`
	FarmID       uint64     `yaml:"farm_id"`
	Kind         string     `yaml:"kind"`
	Model        string     `yaml:"model"`
	Capabilities []string   `yaml:"capabilities"`
	Fields       []string   `yaml:"fields"`
	Status       SpecStatus `yaml:"status"`
}

// SpecStatus 是模拟状态。
type SpecStatus struct {
	Online       bool          `yaml:"online"`
	State        string        `yaml:"state"`
	BatteryPct   *float64      `yaml:"battery_pct,omitempty"`
	TankL        *float64      `yaml:"tank_l,omitempty"`
	FaultCode    string        `yaml:"fault_code,omitempty"`
	HeartbeatLag time.Duration `yaml:"heartbeat_lag"`
}

// SourceName 是模拟平台设备数据的 source（mcp-device real 透传为工具结果 source）。
const SourceName = "devicesim"

// LoadDevices 严格读取设备清单（未知字段报错）。
func LoadDevices(path string) (DeviceFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return DeviceFile{}, fmt.Errorf("sim: read %s: %w", path, err)
	}
	var d DeviceFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return DeviceFile{}, fmt.Errorf("sim: decode %s: %w", path, err)
	}
	if err := d.validate(); err != nil {
		return DeviceFile{}, fmt.Errorf("sim: %s: %w", path, err)
	}
	return d, nil
}

func (d DeviceFile) validate() error {
	var errs []error
	seen := make(map[string]bool, len(d.Devices))
	for i, dv := range d.Devices {
		if dv.DeviceID == "" || seen[dv.DeviceID] {
			errs = append(errs, fmt.Errorf("devices[%d]: device_id empty or duplicate", i))
		}
		seen[dv.DeviceID] = true
		if dv.TenantID == 0 || dv.FarmID == 0 {
			errs = append(errs, fmt.Errorf("devices[%d]: tenant_id and farm_id are required", i))
		}
		if dv.Status.HeartbeatLag < 0 {
			errs = append(errs, fmt.Errorf("devices[%d]: heartbeat_lag must be >= 0", i))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) info(dv DeviceSpec, now time.Time) devicesim.DeviceInfo {
	st := dv.Status
	return devicesim.DeviceInfo{DeviceID: dv.DeviceID, TenantID: dv.TenantID, FarmID: dv.FarmID, Kind: dv.Kind,
		Model: dv.Model, Capabilities: append([]string{}, dv.Capabilities...), Fields: append([]string{}, dv.Fields...),
		Status: devicesim.DeviceStatus{Online: st.Online, State: st.State, BatteryPct: st.BatteryPct, TankL: st.TankL,
			FaultCode: st.FaultCode, LastSeenAt: now.Add(-st.HeartbeatLag).UTC()}}
}

// listDevices 是 GET /v1/devices[?tenant_id=&farm_id=]：两者都给时只返回该农场的设备。
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var tenant, farm uint64
	if q.Has("tenant_id") || q.Has("farm_id") {
		var errT, errF error
		tenant, errT = strconv.ParseUint(q.Get("tenant_id"), 10, 64)
		farm, errF = strconv.ParseUint(q.Get("farm_id"), 10, 64)
		if errT != nil || errF != nil || tenant == 0 || farm == 0 {
			writeErr(w, http.StatusBadRequest, devicesim.CodeBadRequest, "tenant_id and farm_id must be given together")
			return
		}
	}
	now := s.clk.Now()
	out := devicesim.DeviceList{Source: SourceName, UpdatedAt: now.UTC(), Devices: []devicesim.DeviceInfo{}}
	for _, dv := range s.devices.Devices {
		if tenant == 0 || (dv.TenantID == tenant && dv.FarmID == farm) {
			out.Devices = append(out.Devices, s.info(dv, now))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// getDevice 是 GET /v1/devices/{id}：单台设备及实时状态。
func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, dv := range s.devices.Devices {
		if dv.DeviceID == id {
			writeJSON(w, http.StatusOK, s.info(dv, s.clk.Now()))
			return
		}
	}
	writeErr(w, http.StatusNotFound, devicesim.CodeNotFound, "device not found")
}
