package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/clock"
)

// Data 是 configs/mock/device.yaml。
type Data struct {
	Source    string    `yaml:"source"`
	UpdatedAt time.Time `yaml:"updated_at"` // 设备清单的数据时间
	Devices   []Device  `yaml:"devices"`
}

// Device 是一台设备及其模拟状态。
type Device struct {
	DeviceID     string   `yaml:"device_id" json:"device_id"`
	Kind         string   `yaml:"kind" json:"kind"`
	Model        string   `yaml:"model" json:"model"`
	Capabilities []string `yaml:"capabilities" json:"capabilities"`
	Fields       []string `yaml:"fields" json:"-"` // 可服务的地块编码
	Status       Status   `yaml:"status" json:"-"`
}

// Status 是模拟状态；HeartbeatLag 是最近心跳距当前时刻的时长（决定 last_seen_at 与 DataFreshnessSec）。
type Status struct {
	Online       bool          `yaml:"online"`
	State        string        `yaml:"state"`
	BatteryPct   *float64      `yaml:"battery_pct,omitempty"`
	TankL        *float64      `yaml:"tank_l,omitempty"`
	FaultCode    string        `yaml:"fault_code,omitempty"`
	HeartbeatLag time.Duration `yaml:"heartbeat_lag"`
}

func (d Data) validate() error {
	var errs []error
	if d.Source == "" || d.UpdatedAt.IsZero() {
		errs = append(errs, errors.New("source and updated_at are required"))
	}
	seen := make(map[string]bool, len(d.Devices))
	for i, dv := range d.Devices {
		if dv.DeviceID == "" || seen[dv.DeviceID] {
			errs = append(errs, fmt.Errorf("devices[%d]: device_id empty or duplicate", i))
		}
		seen[dv.DeviceID] = true
		if dv.Status.HeartbeatLag < 0 {
			errs = append(errs, fmt.Errorf("devices[%d]: heartbeat_lag must be >= 0", i))
		}
	}
	return errors.Join(errs...)
}

type mock struct {
	data Data
	clk  clock.Clock
	dir  kit.FieldDirectory
}

// LoadMock 读取 mock 数据并构造 mock Server。
func LoadMock(path string, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	var d Data
	if err := kit.LoadYAML(path, &d); err != nil {
		return nil, err
	}
	return NewMock(d, clk, dir)
}

// NewMock 用给定数据构造 mock Server。
func NewMock(d Data, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("device mock data: %w", err)
	}
	m := &mock{data: d, clk: clk, dir: dir}
	return kit.NewServer(Name, Specs(), map[string]kit.Handler{ToolList: m.list, ToolStatus: m.status})
}

type listArgs struct {
	kit.FieldArgs
	Kind string `json:"kind"`
}

func (m *mock) list(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a listArgs
	code, err := kit.DecodeField(ctx, m.dir, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	devices := make([]Device, 0, len(m.data.Devices))
	for _, dv := range m.data.Devices {
		if slices.Contains(dv.Fields, code) && (a.Kind == "" || dv.Kind == a.Kind) {
			devices = append(devices, dv)
		}
	}
	out := map[string]any{"field_id": a.FieldID, "devices": devices}
	return kit.NewResult(out, m.data.Source, m.data.UpdatedAt, m.clk.Now())
}

type statusArgs struct {
	DeviceID string `json:"device_id"`
}

type statusOut struct {
	DeviceID   string   `json:"device_id"`
	Online     bool     `json:"online"`
	State      string   `json:"state"`
	BatteryPct *float64 `json:"battery_pct,omitempty"`
	TankL      *float64 `json:"tank_l,omitempty"`
	FaultCode  string   `json:"fault_code,omitempty"`
	LastSeenAt string   `json:"last_seen_at"`
}

func (m *mock) status(_ context.Context, args json.RawMessage) (mcp.Result, error) {
	var a statusArgs
	if err := kit.DecodeArgs(args, &a); err != nil {
		return mcp.Result{}, err
	}
	if a.DeviceID == "" {
		return mcp.Result{}, kit.Rejected("device_id is required")
	}
	i := slices.IndexFunc(m.data.Devices, func(d Device) bool { return d.DeviceID == a.DeviceID })
	if i < 0 {
		return mcp.Result{}, kit.Empty("device %q not found", a.DeviceID)
	}
	st := m.data.Devices[i].Status
	now := m.clk.Now()
	seen := now.Add(-st.HeartbeatLag)
	out := statusOut{DeviceID: a.DeviceID, Online: st.Online, State: st.State, BatteryPct: st.BatteryPct,
		TankL: st.TankL, FaultCode: st.FaultCode, LastSeenAt: seen.UTC().Format(time.RFC3339)}
	return kit.NewResult(out, m.data.Source, seen, now)
}
