package kit

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"agri-steward/internal/agent/tools/mcp"
)

// RealConfig 是 real 实现的上游配置（来自 configs/tools.yaml 与环境变量）。
type RealConfig struct {
	BaseURL string
	APIKey  string // secret，只来自环境变量
	Timeout time.Duration
}

// RealServer 是 real 实现骨架：工具 schema 与 mock 完全一致，持有 HTTP 客户端与上游配置，
// 但 P0 不调用外网（D6），一律返回 UPSTREAM "real not configured"。对接时只替换 Invoke 内部。
type RealServer struct {
	name   string
	specs  []mcp.ToolSpec
	cfg    RealConfig
	client *http.Client
}

// ErrRealNotConfigured 是 real 骨架的统一返回。
const ErrRealNotConfigured = "real not configured"

// NewRealServer 构造 real 骨架；HTTP 客户端超时取配置超时。
func NewRealServer(name string, specs []mcp.ToolSpec, cfg RealConfig) *RealServer {
	return &RealServer{name: name, specs: specs, cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

// Name 实现 mcp.Server。
func (s *RealServer) Name() string { return s.name }

// Tools 实现 mcp.Server。
func (s *RealServer) Tools() []mcp.ToolSpec { return append([]mcp.ToolSpec(nil), s.specs...) }

// Invoke 实现 mcp.Server：未对接上游，返回 UPSTREAM。
func (s *RealServer) Invoke(context.Context, string, json.RawMessage) (mcp.Result, error) {
	return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrUpstream, Msg: ErrRealNotConfigured}
}
