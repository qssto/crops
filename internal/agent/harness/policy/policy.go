// Package policy 是工具调用的前后置策略（P0 §6.3 流程 1、4 的最小版）：
// Before 做等级校验与入参 schema 校验，After 校验标准化结果的必备字段。
// L5 起改为读 policy/tool_policy.yaml 并补 actuate 兜底；本包不依赖 mcp，工具声明由调用方转换成 Spec。
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Level 是工具策略等级。
type Level string

const (
	LevelRead    Level = "read"
	LevelWrite   Level = "write"
	LevelActuate Level = "actuate"
)

// Validator 校验解码后的入参（JSON 数字为 json.Number）；*jsonschema.Schema 满足此接口。
type Validator interface {
	Validate(v any) error
}

// Spec 是一个工具的策略声明。
type Spec struct {
	Tool  string
	Level Level     // 空串表示未声明等级，Before 一律拒绝
	Args  Validator // 入参 JSON Schema
}

// Before 拒绝的原因（均对应 REJECTED）。
var (
	ErrUnknownTool     = errors.New("policy: tool not declared")
	ErrLevelUndeclared = errors.New("policy: tool level not declared")
	ErrLevelDenied     = errors.New("policy: tool level not allowed")
	ErrBadArgs         = errors.New("policy: args rejected")
)

// Policy 是按工具名索引的策略表，构造后只读，并发安全。
type Policy struct {
	specs   map[string]Spec
	allowed map[Level]bool
	actuate map[string]bool // ToolPolicy.Bind 写入：策略声明为 actuate 的工具名（未注册也兜底拒绝）
}

// New 构造策略表：工具名为空、重名或缺入参校验器都报错；allowed 是允许调用的等级。
func New(specs []Spec, allowed ...Level) (*Policy, error) {
	p := &Policy{specs: make(map[string]Spec, len(specs)), allowed: make(map[Level]bool, len(allowed))}
	var errs []error
	for _, s := range specs {
		switch {
		case s.Tool == "":
			errs = append(errs, errors.New("policy: empty tool name"))
		case s.Args == nil:
			errs = append(errs, fmt.Errorf("policy: %s: args validator is required", s.Tool))
		default:
			if _, dup := p.specs[s.Tool]; dup {
				errs = append(errs, fmt.Errorf("policy: duplicate tool %q", s.Tool))
				continue
			}
			p.specs[s.Tool] = s
		}
	}
	for _, l := range allowed {
		p.allowed[l] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// Before 校验工具已声明、等级已声明且被允许、入参是合法 JSON 对象并符合 schema；
// 通过返回该工具的 Spec，失败返回包装了上述哨兵错误的 error。空入参视为 {}。
// actuate 工具（已注册或策略声明）一律返回 ErrActuateDenied，与 allowed 无关。
func (p *Policy) Before(tool string, args json.RawMessage) (Spec, error) {
	s, ok := p.specs[tool]
	if p.actuate[tool] || (ok && s.Level == LevelActuate) {
		return Spec{}, actuateDenied(tool)
	}
	if !ok {
		return Spec{}, fmt.Errorf("%w: %q", ErrUnknownTool, tool)
	}
	if s.Level == "" {
		return Spec{}, fmt.Errorf("%w: %s", ErrLevelUndeclared, tool)
	}
	if !p.allowed[s.Level] {
		return Spec{}, fmt.Errorf("%w: %s is %s", ErrLevelDenied, tool, s.Level)
	}
	v, err := decodeObject(args)
	if err != nil {
		return Spec{}, fmt.Errorf("%w: %s: %v", ErrBadArgs, tool, err)
	}
	if err := s.Args.Validate(v); err != nil {
		return Spec{}, fmt.Errorf("%w: %s: %v", ErrBadArgs, tool, err)
	}
	return s, nil
}

// decodeObject 把入参解码为 JSON 对象（数字保留为 json.Number），拒绝非对象与尾随内容。
func decodeObject(args json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("args must be a JSON object: %w", err)
	}
	if m == nil {
		return nil, errors.New("args must be a JSON object, got null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after args")
	}
	return m, nil
}

// Outcome 是 After 检查的标准化结果（tool_call.status / error_code / source）。
type Outcome struct {
	Status    string
	ErrorCode string
	Source    string
}

// 与 tool_call.status、错误码一致（不 import mcp / model，保持本包只依赖标准库）。
const (
	statusOK        = "OK"
	statusError     = "ERROR"
	codeMalformed   = "MALFORMED"
	codeMissingCode = "UPSTREAM"
)

// After 校验结果必备字段：OK 必须带数据源（Citation 依赖），否则降为 ERROR / MALFORMED；
// 非 OK 必须带错误码，缺失补 UPSTREAM。返回修正后的结果与修正原因（未修正为空串）。
func (p *Policy) After(tool string, o Outcome) (Outcome, string) {
	switch {
	case o.Status == statusOK && o.Source == "":
		return Outcome{Status: statusError, ErrorCode: codeMalformed}, tool + ": OK result without source"
	case o.Status != statusOK && o.ErrorCode == "":
		o.ErrorCode = codeMissingCode
		return o, tool + ": failed result without error code"
	default:
		return o, ""
	}
}
