// Package servers 按 configs/tools.yaml 装配四个 MCP Server：每个 Server 按 mode 选 mock 或 real，
// mock → real 切换只改配置，不改业务代码。
package servers

import (
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
)

// mockLoader 读取 {mock_dir}/{server}.yaml 构造 mock Server。
type mockLoader func(path string, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error)

// realFactory 构造 real 骨架。
type realFactory func(cfg kit.RealConfig) *kit.RealServer

type entry struct {
	mock mockLoader
	real realFactory
}

// registry 是 server 名 → 实现，名字与 config.Tools.Servers() 一致。
var registry = map[string]entry{
	weather.Name: {mock: weather.LoadMock, real: weather.NewReal},
	crop.Name:    {mock: crop.LoadMock, real: crop.NewReal},
	field.Name:   {mock: field.LoadMock, real: field.NewReal},
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
		return e.real(kit.RealConfig{BaseURL: s.Real.BaseURL, APIKey: s.Real.APIKey, Timeout: s.Timeout}), nil
	default:
		return nil, fmt.Errorf("servers: %s: unknown mode %q", s.Name, s.Mode)
	}
}
