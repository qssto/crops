package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/model"
)

// FaultMatrixVersion 是本包支持的 fault_matrix.yaml 版本。
const FaultMatrixVersion = 1

// DecisionPlaceholder 是降级模板里规则层决策句的占位符。
const DecisionPlaceholder = "{decision}"

// ErrBadFaultMatrix 是 fault_matrix.yaml 不合法或与文案表不对应（加载期，启动失败）。
var ErrBadFaultMatrix = errors.New("policy: invalid fault_matrix")

// 告警级别（L7 接告警规则）。
const (
	AlertCritical = "critical"
	AlertWarning  = "warning"
)

// MCP 网关错误码（与 mcp.ErrTimeout / mcp.ErrUpstream 同值；mcp 包有一致性测试，本包不反向依赖 mcp）。
const (
	toolErrTimeout  = "TIMEOUT"
	toolErrUpstream = "UPSTREAM"
)

// Alert 是故障对应的告警项（先只有名字与级别）。
type Alert struct {
	Name  string `yaml:"name"`
	Level string `yaml:"level"`
}

// Fault 是一类故障的声明：行为、无法降级时的拒答原因、文案与告警。
type Fault struct {
	Code         model.FaultCode
	Behavior     model.DegradeAction
	RefuseReason string   // advice_card.refuse_reason；DEVICE_OFFLINE 为空（不出卡）
	ToolErrors   []string // 归入本故障的 MCP 错误码（仅 MCP_TIMEOUT）
	TextKey      string
	Text         string
	// DegradedTextKey / DegradedText 是规则层降级模板（仅 LLM_UNAVAILABLE），含一个 DecisionPlaceholder。
	DegradedTextKey string
	DegradedText    string
	Alert           Alert
}

// Degraded 用规则层决策句填充降级模板。
func (f Fault) Degraded(decision string) string {
	return strings.Replace(f.DegradedText, DecisionPlaceholder, decision, 1)
}

// faultSpec 是代码实现的行为：矩阵声明必须与之一致。
type faultSpec struct {
	behavior     model.DegradeAction
	refuseReason string
	degraded     bool // 需要 degraded_text_key
	toolErrors   []string
}

var faultSpecs = map[model.FaultCode]faultSpec{
	model.FaultLLMUnavailable: {behavior: model.DegradeRuleFallback, refuseReason: "LLM_UNAVAILABLE", degraded: true},
	model.FaultMCPTimeout: {behavior: model.DegradeCacheFallback, refuseReason: "DATA_MISSING",
		toolErrors: []string{toolErrTimeout, toolErrUpstream}},
	model.FaultKBMiss:        {behavior: model.DegradeRefuse, refuseReason: "KB_MISS"},
	model.FaultDeviceOffline: {behavior: model.DegradeHoldNotify},
}

// FaultMatrix 是 policy/fault_matrix.yaml 与其文案。构造后只读，并发安全。
type FaultMatrix struct {
	faults   map[model.FaultCode]Fault
	byReason map[string]model.FaultCode
	byTool   map[string]model.FaultCode
}

type faultMatrixFile struct {
	Version int                           `yaml:"version"`
	Faults  map[model.FaultCode]faultDecl `yaml:"faults"`
}

type faultDecl struct {
	Behavior        model.DegradeAction `yaml:"behavior"`
	RefuseReason    string              `yaml:"refuse_reason"`
	ToolErrorCodes  []string            `yaml:"tool_error_codes"`
	TextKey         string              `yaml:"text_key"`
	DegradedTextKey string              `yaml:"degraded_text_key"`
	Alert           Alert               `yaml:"alert"`
}

// LoadFaultMatrix 读取故障矩阵与文案表并交叉校验（cmd/server、cmd/worker、cmd/eval 共用的启动校验）。
func LoadFaultMatrix(matrixPath, textsPath string) (*FaultMatrix, error) {
	texts, err := LoadTexts(textsPath)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(matrixPath)
	if err != nil {
		return nil, fmt.Errorf("policy: read fault_matrix: %w", err)
	}
	m, err := ParseFaultMatrix(b, texts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", matrixPath, err)
	}
	return m, nil
}

// ParseFaultMatrix 解析矩阵并按文案表校验：四个码齐全且无未知码；behavior / refuse_reason / tool_error_codes 与实现一致；
// 每个码有 text_key（LLM_UNAVAILABLE 另有 degraded_text_key）且文案存在、不含数字；告警 name 非空、level 合法；
// text_key 与告警 name 各故障间不重复。所有问题一次列全。
func ParseFaultMatrix(b []byte, texts *Texts) (*FaultMatrix, error) {
	if texts == nil {
		return nil, fmt.Errorf("%w: texts are required", ErrBadFaultMatrix)
	}
	var f faultMatrixFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadFaultMatrix, err)
	}
	if f.Version != FaultMatrixVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadFaultMatrix, f.Version, FaultMatrixVersion)
	}
	v := &faultValidator{texts: texts, keys: map[string]model.FaultCode{}, alerts: map[string]model.FaultCode{}}
	for code := range f.Faults {
		if _, ok := faultSpecs[code]; !ok {
			v.bad("unknown fault %q", code)
		}
	}
	m := &FaultMatrix{faults: map[model.FaultCode]Fault{}, byReason: map[string]model.FaultCode{},
		byTool: map[string]model.FaultCode{}}
	for _, code := range model.FaultCodes() {
		d, ok := f.Faults[code]
		if !ok {
			v.bad("%s: missing", code)
			continue
		}
		fault := v.fault(code, d)
		m.faults[code] = fault
		if fault.RefuseReason != "" {
			m.byReason[fault.RefuseReason] = code
		}
		for _, te := range fault.ToolErrors {
			m.byTool[te] = code
		}
	}
	if err := errors.Join(v.errs...); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadFaultMatrix, err)
	}
	return m, nil
}
