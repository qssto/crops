// Package kit 是四个 MCP Server（weather / crop / field / device）的公共骨架：
// 按工具名分发、严格解码入参、组装带 Source / DataFreshnessSec 的标准返回、读取 mock 数据。
package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent/tools/mcp"
)

// Handler 处理一个工具调用，args 是模型给出的 JSON 入参。
type Handler func(ctx context.Context, args json.RawMessage) (mcp.Result, error)

// Server 是按工具名分发的 mcp.Server 实现，mock 与 real 共用。
type Server struct {
	name     string
	specs    []mcp.ToolSpec
	handlers map[string]Handler
}

// NewServer 校验 specs 与 handlers 一一对应（工具名须以 "{name}." 开头），不一致即报错，
// 保证每个声明的工具都有实现、每个实现都有 schema。
func NewServer(name string, specs []mcp.ToolSpec, handlers map[string]Handler) (*Server, error) {
	var errs []error
	seen := make(map[string]bool, len(specs))
	for _, sp := range specs {
		switch {
		case !strings.HasPrefix(sp.Name, name+"."):
			errs = append(errs, fmt.Errorf("tool %q must be prefixed with %q", sp.Name, name+"."))
		case seen[sp.Name]:
			errs = append(errs, fmt.Errorf("duplicate tool %q", sp.Name))
		case handlers[sp.Name] == nil:
			errs = append(errs, fmt.Errorf("tool %q has no handler", sp.Name))
		}
		seen[sp.Name] = true
	}
	for tool := range handlers {
		if !seen[tool] {
			errs = append(errs, fmt.Errorf("handler %q has no spec", tool))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("mcp server %s: %w", name, err)
	}
	return &Server{name: name, specs: specs, handlers: handlers}, nil
}

// Name 实现 mcp.Server。
func (s *Server) Name() string { return s.name }

// Tools 实现 mcp.Server，返回副本防止调用方改动注册信息。
func (s *Server) Tools() []mcp.ToolSpec { return append([]mcp.ToolSpec(nil), s.specs...) }

// Invoke 实现 mcp.Server：未注册的工具返回 REJECTED。
func (s *Server) Invoke(ctx context.Context, tool string, args json.RawMessage) (mcp.Result, error) {
	h, ok := s.handlers[tool]
	if !ok {
		return mcp.Result{}, &mcp.ToolError{Code: mcp.ErrRejected, Msg: fmt.Sprintf("%s: unknown tool %q", s.name, tool)}
	}
	if err := ctx.Err(); err != nil {
		return mcp.Result{}, fmt.Errorf("%s: %w", tool, err)
	}
	return h(ctx, args)
}

// Rejected 构造入参非法的错误。
func Rejected(format string, a ...any) error {
	return &mcp.ToolError{Code: mcp.ErrRejected, Msg: fmt.Sprintf(format, a...)}
}

// Empty 构造上游无数据的错误（下游据此走 DATA_MISSING 拒答）。
func Empty(format string, a ...any) error {
	return &mcp.ToolError{Code: mcp.ErrEmpty, Msg: fmt.Sprintf(format, a...)}
}

// DecodeArgs 严格解码入参：未知字段、类型不符、多余内容均返回 REJECTED。空入参视为 {}。
func DecodeArgs(args json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Rejected("bad args: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Rejected("bad args: trailing data")
	}
	return nil
}

// FieldArgs 是按地块查询的公共入参：field_id 取 field_profile.id，模型入参不带坐标。
type FieldArgs struct {
	FieldID uint64 `json:"field_id"`
}

// Require 校验 field_id 已给出。
func (a FieldArgs) Require() error {
	if a.FieldID == 0 {
		return Rejected("field_id is required and must be >= 1")
	}
	return nil
}

// NewResult 序列化 data 并计算数据新鲜度（now - collectedAt，未来时刻按 0 计），时间统一 UTC。
func NewResult(data any, source string, collectedAt, now time.Time) (mcp.Result, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return mcp.Result{}, fmt.Errorf("marshal tool data: %w", err)
	}
	fresh := int(now.Sub(collectedAt) / time.Second)
	if fresh < 0 {
		fresh = 0
	}
	return mcp.Result{Data: raw, Source: source, DataFreshnessSec: fresh,
		CollectedAt: collectedAt.UTC().Format(time.RFC3339)}, nil
}

// LoadYAML 严格读取 mock 数据文件（未知字段报错，防止拼写错误静默生效）。
func LoadYAML(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mock data: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("mock data: decode %s: %w", path, err)
	}
	return nil
}

// ParseDate 解析 YYYY-MM-DD；空串返回 def。
func ParseDate(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, Rejected("date %q must be YYYY-MM-DD", s)
	}
	return d, nil
}

// SortedNames 返回 specs 的工具名（排序），供清单与测试使用。
func SortedNames(specs []mcp.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
