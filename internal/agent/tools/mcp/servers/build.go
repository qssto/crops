// Package servers 按 configs/tools.yaml 装配四个 MCP Server：每个 Server 按 mode 选 mock 或 real，
// mock → real 切换只改配置，不改业务代码。
package servers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/crop"
	"agri-steward/internal/agent/tools/mcp/servers/device"
	"agri-steward/internal/agent/tools/mcp/servers/field"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/agent/tools/mcp/servers/weather"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
)

// mockLoader 读取 {mock_dir}/{server}.yaml 构造 mock Server。
type mockLoader func(path string, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error)

// realFactory 构造 real Server（weather / crop / field 仍为骨架；device 配了 base_url 时读设备平台）。
type realFactory func(cfg kit.RealConfig, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error)

// skeleton 把只需配置的 real 骨架构造函数适配为 realFactory。
func skeleton(f func(kit.RealConfig) *kit.RealServer) realFactory {
	return func(cfg kit.RealConfig, _ clock.Clock, _ kit.FieldDirectory) (mcp.Server, error) { return f(cfg), nil }
}

type entry struct {
	mock mockLoader
	real realFactory
}

// registry 是 server 名 → 实现，名字与 config.Tools.Servers() 一致。
var registry = map[string]entry{
	weather.Name: {mock: weather.LoadMock, real: skeleton(weather.NewReal)},
	crop.Name:    {mock: crop.LoadMock, real: skeleton(crop.NewReal)},
	field.Name:   {mock: field.LoadMock, real: skeleton(field.NewReal)},
	device.Name:  {mock: device.LoadMock, real: device.NewReal},
}

// Build 按配置装配全部 Server（顺序同 config.Tools.Servers()）。dir 把 field_id 翻成地块编码，供 mock 查数据。
func Build(cfg *config.Tools, clk clock.Clock, dir kit.FieldDirectory) ([]mcp.Server, error) {
	if cfg == nil || clk == nil || dir == nil {
		return nil, errors.New("servers: cfg, clock and field directory are required")
	}
	named := cfg.Servers()
	out := make([]mcp.Server, 0, len(named))
	var errs []error
	for _, s := range named {
		srv, err := build(cfg.MockDir, s, clk, dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, srv)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// DeviceBindings 是 gateway 的设备归属（agent.DeviceOwnership）加评测按结构判定用的本农场设备清单。
type DeviceBindings interface {
	OwnsDevice(ctx context.Context, scope model.Scope, deviceID string) (bool, error)
	DeviceIDs(ctx context.Context, scope model.Scope) ([]string, error)
}

// DeviceOwnership 按 device server 的 mode 构造设备归属（gateway Options.Devices）：mock 读 {mock_dir}/device.yaml 的显式绑定；
// real 配了 base_url 时按设备平台返回的本农场设备清单判定（P1 L2：devicesim），未配为空绑定、全部 SCOPE_DENIED（fail closed）。
func DeviceOwnership(cfg *config.Tools) (DeviceBindings, error) {
	if cfg == nil {
		return nil, errors.New("servers: cfg is required")
	}
	switch cfg.Device.Mode {
	case config.ToolModeMock:
		b, err := device.LoadBindings(filepath.Join(cfg.MockDir, device.Name+".yaml"))
		if err != nil {
			return nil, fmt.Errorf("servers: device bindings: %w", err)
		}
		return b, nil
	case config.ToolModeReal:
		if cfg.Device.Real.BaseURL == "" {
			return device.NewBindings(nil), nil
		}
		p, err := device.NewPlatform(realConfig(config.NamedToolServer{Name: device.Name, ToolServer: cfg.Device}))
		if err != nil {
			return nil, fmt.Errorf("servers: device bindings: %w", err)
		}
		return device.NewRemoteBindings(p), nil
	default:
		return nil, fmt.Errorf("servers: device: unknown mode %q", cfg.Device.Mode)
	}
}

// realConfig 取 real 上游配置（超时与 mock 同一项）。
func realConfig(s config.NamedToolServer) kit.RealConfig {
	return kit.RealConfig{BaseURL: s.Real.BaseURL, APIKey: s.Real.APIKey, Timeout: s.Timeout}
}

func build(mockDir string, s config.NamedToolServer, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	e, ok := registry[s.Name]
	if !ok {
		return nil, fmt.Errorf("servers: unknown server %q", s.Name)
	}
	switch s.Mode {
	case config.ToolModeMock:
		srv, err := e.mock(filepath.Join(mockDir, s.Name+".yaml"), clk, dir)
		if err != nil {
			return nil, fmt.Errorf("servers: %s mock: %w", s.Name, err)
		}
		return srv, nil
	case config.ToolModeReal:
		srv, err := e.real(realConfig(s), clk, dir)
		if err != nil {
			return nil, fmt.Errorf("servers: %s real: %w", s.Name, err)
		}
		return srv, nil
	default:
		return nil, fmt.Errorf("servers: %s: unknown mode %q", s.Name, s.Mode)
	}
}
