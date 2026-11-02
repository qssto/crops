package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/clock"
	"agri-steward/internal/model"
)

// platformDevice 是设备平台 GET /v1/devices 的一项（本地为 devicesim，见 internal/adapter/devicesim.DeviceInfo；
// 本包不 import adapter，字段按协议自持）。
type platformDevice struct {
	DeviceID     string   `json:"device_id"`
	TenantID     uint64   `json:"tenant_id"`
	FarmID       uint64   `json:"farm_id"`
	Kind         string   `json:"kind"`
	Model        string   `json:"model"`
	Capabilities []string `json:"capabilities"`
	Fields       []string `json:"fields"`
	Status       struct {
		Online     bool      `json:"online"`
		State      string    `json:"state"`
		BatteryPct *float64  `json:"battery_pct,omitempty"`
		TankL      *float64  `json:"tank_l,omitempty"`
		FaultCode  string    `json:"fault_code,omitempty"`
		LastSeenAt time.Time `json:"last_seen_at"`
	} `json:"status"`
}

// platformList 是 GET /v1/devices?tenant_id=&farm_id= 的响应：只含该农场显式绑定的设备。
type platformList struct {
	Source    string           `json:"source"`
	UpdatedAt time.Time        `json:"updated_at"`
	Devices   []platformDevice `json:"devices"`
}

// maxPlatformBody 是设备清单响应体上限（1 MiB）。
const maxPlatformBody = 1 << 20

// Platform 是设备平台的只读 HTTP 客户端（mcp-device real 与 real 模式设备归属共用）。
type Platform struct {
	base   string
	client *http.Client
	log    *slog.Logger
}

// NewPlatform 校验 base_url（http / https 且带主机）并构造客户端，超时取配置超时。
func NewPlatform(cfg kit.RealConfig) (*Platform, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("device: real base_url %q must be an http(s) URL", cfg.BaseURL)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("device: real timeout must be > 0")
	}
	return &Platform{base: strings.TrimRight(cfg.BaseURL, "/"), client: &http.Client{Timeout: cfg.Timeout},
		log: slog.Default()}, nil
}

// farmDevices 取 scope 农场的设备清单；scope 缺租户或农场直接 SCOPE_DENIED（不发请求）。
// 超时 → TIMEOUT，连接失败 / 非 200 → UPSTREAM，响应非 JSON → MALFORMED。
func (p *Platform) farmDevices(ctx context.Context, scope model.Scope) (platformList, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 {
		return platformList{}, kit.ScopeDenied("device: scope tenant_id/farm_id required")
	}
	q := url.Values{"tenant_id": {strconv.FormatUint(scope.TenantID, 10)}, "farm_id": {strconv.FormatUint(scope.FarmID, 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/v1/devices?"+q.Encode(), nil)
	if err != nil {
		return platformList{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: fmt.Sprintf("device platform request: %v", err)}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return platformList{}, &mcp.ToolError{Code: mcp.ErrTimeout, Msg: fmt.Sprintf("device platform: %v", err)}
		}
		return platformList{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: fmt.Sprintf("device platform: %v", err)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPlatformBody))
	if err = errors.Join(err, resp.Body.Close()); err != nil {
		return platformList{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: fmt.Sprintf("device platform read: %v", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return platformList{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: fmt.Sprintf("device platform: HTTP %d", resp.StatusCode)}
	}
	var out platformList
	if err := json.Unmarshal(raw, &out); err != nil {
		return platformList{}, &mcp.ToolError{Code: mcp.ErrMalformed, Msg: fmt.Sprintf("device platform body: %v", err)}
	}
	if out.Source == "" {
		return platformList{}, &mcp.ToolError{Code: mcp.ErrMalformed, Msg: "device platform body: source is required"}
	}
	if err := p.checkOwnership(ctx, scope, out.Devices); err != nil {
		return platformList{}, err
	}
	return out, nil
}

// checkOwnership 校验平台返回的每台设备都属于 scope 农场。查询参数过滤由平台执行，不可信：
// 只要出现一台他场设备即整体失败关闭（UPSTREAM）并告警，而不是剔除后继续——平台串数据时同一响应里
// 本场设备的状态也不可信，且剔除会把平台故障静默成「设备变少」。
func (p *Platform) checkOwnership(ctx context.Context, scope model.Scope, devices []platformDevice) error {
	for _, d := range devices {
		if d.TenantID == scope.TenantID && d.FarmID == scope.FarmID {
			continue
		}
		p.log.WarnContext(ctx, "device platform returned device outside requested farm",
			"device_id", d.DeviceID, "want_tenant_id", scope.TenantID, "want_farm_id", scope.FarmID,
			"got_tenant_id", d.TenantID, "got_farm_id", d.FarmID)
		return &mcp.ToolError{Code: mcp.ErrUpstream,
			Msg: fmt.Sprintf("device platform returned device %q outside farm (%d,%d)", d.DeviceID, scope.TenantID, scope.FarmID)}
	}
	return nil
}

// RemoteBindings 是 real 模式的设备归属：按平台返回的 scope 农场设备清单判定（平台按显式绑定过滤）。
type RemoteBindings struct{ p *Platform }

// NewRemoteBindings 构造 real 模式设备归属。
func NewRemoteBindings(p *Platform) *RemoteBindings { return &RemoteBindings{p: p} }

// OwnsDevice 实现 agent.DeviceOwnership：scope 缺租户或农场、设备不在本农场清单一律 false；平台不可用返回错误。
func (b *RemoteBindings) OwnsDevice(ctx context.Context, scope model.Scope, deviceID string) (bool, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 {
		return false, nil
	}
	list, err := b.p.farmDevices(ctx, scope)
	if err != nil {
		return false, fmt.Errorf("device ownership: %w", err)
	}
	return slices.ContainsFunc(list.Devices, func(d platformDevice) bool { return d.DeviceID == deviceID }), nil
}

// DeviceIDs 返回平台绑定到 scope 农场的设备 ID（升序）。
func (b *RemoteBindings) DeviceIDs(ctx context.Context, scope model.Scope) ([]string, error) {
	if scope.TenantID == 0 || scope.FarmID == 0 {
		return []string{}, nil
	}
	list, err := b.p.farmDevices(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("device ids: %w", err)
	}
	out := make([]string, 0, len(list.Devices))
	for _, d := range list.Devices {
		out = append(out, d.DeviceID)
	}
	slices.Sort(out)
	return out, nil
}

// realServer 从设备平台取设备清单与实时状态（在线 / 续航 / 载药 / 能力），输出与 mock 同一 schema。
type realServer struct {
	p   *Platform
	clk clock.Clock
	dir kit.FieldDirectory
}

// NewReal 构造 device real Server：base_url 为空时保持骨架（一律 UPSTREAM "real not configured"）；
// 否则经 HTTP 读设备平台（本地 / 预发为 devicesim）。mock / real 只由 configs/tools.yaml 切换。
func NewReal(cfg kit.RealConfig, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	if cfg.BaseURL == "" {
		return kit.NewRealServer(Name, Specs(), cfg), nil
	}
	p, err := NewPlatform(cfg)
	if err != nil {
		return nil, err
	}
	if clk == nil || dir == nil {
		return nil, errors.New("device: real requires clock and field directory")
	}
	r := &realServer{p: p, clk: clk, dir: dir}
	return kit.NewServer(Name, Specs(), map[string]kit.Handler{ToolList: r.list, ToolStatus: r.status})
}

func (r *realServer) list(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a listArgs
	code, err := kit.DecodeField(ctx, r.dir, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	pl, err := r.p.farmDevices(ctx, mcp.ScopeFrom(ctx))
	if err != nil {
		return mcp.Result{}, err
	}
	if pl.UpdatedAt.IsZero() {
		return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrMalformed, Msg: "device platform: list missing updated_at (采集时间缺失)"}
	}
	devices := make([]Device, 0, len(pl.Devices))
	for _, d := range pl.Devices {
		if slices.Contains(d.Fields, code) && (a.Kind == "" || d.Kind == a.Kind) {
			devices = append(devices, Device{DeviceID: d.DeviceID, Kind: d.Kind, Model: d.Model, Capabilities: d.Capabilities})
		}
	}
	return kit.NewResult(map[string]any{"field_id": a.FieldID, "devices": devices}, pl.Source, pl.UpdatedAt, r.clk.Now())
}

func (r *realServer) status(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a statusArgs
	if err := kit.DecodeArgs(args, &a); err != nil {
		return mcp.Result{}, err
	}
	if a.DeviceID == "" {
		return mcp.Result{}, kit.Rejected("device_id is required")
	}
	pl, err := r.p.farmDevices(ctx, mcp.ScopeFrom(ctx))
	if err != nil {
		return mcp.Result{}, err
	}
	i := slices.IndexFunc(pl.Devices, func(d platformDevice) bool { return d.DeviceID == a.DeviceID })
	if i < 0 {
		return mcp.Result{}, kit.ScopeDenied("device %q not in farm", a.DeviceID)
	}
	d := pl.Devices[i]
	st := d.Status
	if st.LastSeenAt.IsZero() {
		// 采集时间缺失：不透传 0001 年时间（否则新鲜度算成上千年或被当作「刚采集」），按平台响应不合规报错。
		return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrMalformed,
			Msg: fmt.Sprintf("device platform: device %q status missing last_seen_at (采集时间缺失)", d.DeviceID)}
	}
	out := statusOut{DeviceID: d.DeviceID, Online: st.Online, State: st.State, BatteryPct: st.BatteryPct, TankL: st.TankL,
		FaultCode: st.FaultCode, LastSeenAt: st.LastSeenAt.UTC().Format(time.RFC3339), Capabilities: d.Capabilities}
	return kit.NewResult(out, pl.Source, st.LastSeenAt, r.clk.Now())
}
