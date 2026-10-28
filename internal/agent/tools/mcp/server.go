// Package mcp 是 MCP 数据接入层：gateway（注册表、超时重试、去重缓存、错误码归一）与工具 Server 契约。
package mcp

import (
	"context"
	"encoding/json"
)

// Level 是工具策略等级（policy/tool_policy.yaml，L5 补 actuate 兜底）。
type Level string

const (
	LevelRead    Level = "read"
	LevelWrite   Level = "write"
	LevelActuate Level = "actuate"
)

// ToolSpec 是工具的 schema 自描述，Server 启动时向 gateway 注册。
type ToolSpec struct {
	Name          string          // 全名 {server}.{tool}，如 weather.forecast
	Level         Level           // 策略等级，policy.Before 校验；gateway Options.Levels 可按 server 覆盖
	Description   string          // 给模型看的用途说明
	InputSchema   json.RawMessage // JSON Schema（object）
	OutputSchema  json.RawMessage // JSON Schema（object），data 部分
	Idempotent    bool            // 只读且幂等：超时可重试 2 次
	Cacheable     bool            // 可走 cache:mcp
	TimeSensitive bool            // 天气、设备状态：恢复与复检换 epoch 重取（默认值，L6 起以 tool_policy.yaml 为准）
}

// Result 是 Server 的标准化返回：每个工具都带数据源与新鲜度，供 Citation 与拒答判断使用。
type Result struct {
	Data             json.RawMessage `json:"data"`
	Source           string          `json:"source"`
	DataFreshnessSec int             `json:"data_freshness_sec"`
	CollectedAt      string          `json:"collected_at"` // RFC3339 UTC
}

// Server 是一个 MCP Server（weather / crop / field / device）。mock 与 real 实现同一接口，配置切换。
// Invoke 的错误用 *ToolError 表达可归一的失败（EMPTY、MALFORMED、UPSTREAM）；
// 其余 error（含 ctx 超时）由 gateway 归一。
type Server interface {
	Name() string
	Tools() []ToolSpec
	Invoke(ctx context.Context, tool string, args json.RawMessage) (Result, error)
}

// ErrorCode 是归一后的工具错误码。
type ErrorCode string

const (
	ErrTimeout   ErrorCode = "TIMEOUT"
	ErrEmpty     ErrorCode = "EMPTY"
	ErrMalformed ErrorCode = "MALFORMED"
	ErrUpstream  ErrorCode = "UPSTREAM"
	ErrRejected  ErrorCode = "REJECTED"
)

// ToolError 是带归一错误码的工具失败。
type ToolError struct {
	Code ErrorCode
	Msg  string
}

func (e *ToolError) Error() string { return string(e.Code) + ": " + e.Msg }
