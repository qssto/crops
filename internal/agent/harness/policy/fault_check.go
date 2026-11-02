package policy

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"agri-steward/internal/model"
)

// faultValidator 逐条校验 fault_matrix 声明并累计问题（一次列全）；keys / alerts 用于检查各故障间不重复。
type faultValidator struct {
	texts  *Texts
	keys   map[string]model.FaultCode
	alerts map[string]model.FaultCode
	errs   []error
}

func (v *faultValidator) bad(format string, args ...any) {
	v.errs = append(v.errs, fmt.Errorf(format, args...))
}

// fault 校验一条声明并组装 Fault：行为与实现一致、文案齐全、告警合法。
func (v *faultValidator) fault(code model.FaultCode, d faultDecl) Fault {
	spec := faultSpecs[code]
	if d.Behavior != spec.behavior {
		v.bad("%s: behavior %q, implementation is %q", code, d.Behavior, spec.behavior)
	}
	if d.RefuseReason != spec.refuseReason {
		v.bad("%s: refuse_reason %q, implementation is %q", code, d.RefuseReason, spec.refuseReason)
	}
	if !sameSet(d.ToolErrorCodes, spec.toolErrors) {
		v.bad("%s: tool_error_codes %v, want %v", code, d.ToolErrorCodes, spec.toolErrors)
	}
	f := Fault{Code: code, Behavior: d.Behavior, RefuseReason: d.RefuseReason, ToolErrors: slices.Clone(d.ToolErrorCodes),
		TextKey: d.TextKey, DegradedTextKey: d.DegradedTextKey, Alert: d.Alert}
	f.Text = v.text(code, "text_key", d.TextKey)
	switch {
	case spec.degraded:
		f.DegradedText = v.text(code, "degraded_text_key", d.DegradedTextKey)
		if f.DegradedText != "" && strings.Count(f.DegradedText, DecisionPlaceholder) != 1 {
			v.bad("%s: %s must contain %s exactly once", code, d.DegradedTextKey, DecisionPlaceholder)
		}
	case d.DegradedTextKey != "":
		v.bad("%s: degraded_text_key is not used by %s", code, d.Behavior)
	}
	v.alert(code, d.Alert)
	return f
}

// text 校验文案 key 非空、未被其他故障占用、文案存在且不含数字，返回文案（失败返回空串）。
func (v *faultValidator) text(code model.FaultCode, field, key string) string {
	if key == "" {
		v.bad("%s: %s is required", code, field)
		return ""
	}
	if prev, dup := v.keys[key]; dup {
		v.bad("%s: %s %q already used by %s", code, field, key, prev)
	}
	v.keys[key] = code
	s, ok := v.texts.Get(key)
	if !ok {
		v.bad("%s: %s %q has no %s text", code, field, key, v.texts.Locale())
		return ""
	}
	if strings.ContainsFunc(s, unicode.IsDigit) {
		v.bad("%s: text %q must not contain digits", code, key)
	}
	return s
}

// alert 校验告警项：name 非空且不重复，level 为 critical | warning。
func (v *faultValidator) alert(code model.FaultCode, a Alert) {
	if a.Name == "" {
		v.bad("%s: alert.name is required", code)
	} else if prev, dup := v.alerts[a.Name]; dup {
		v.bad("%s: alert %q already used by %s", code, a.Name, prev)
	} else {
		v.alerts[a.Name] = code
	}
	if a.Level != AlertCritical && a.Level != AlertWarning {
		v.bad("%s: alert.level %q, want %s | %s", code, a.Level, AlertCritical, AlertWarning)
	}
}

// sameSet 比较两个错误码列表（忽略顺序）。
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	return slices.Equal(g, w)
}

// Fault 返回故障码的声明；未知码返回零值与 false。
func (m *FaultMatrix) Fault(code model.FaultCode) (Fault, bool) {
	f, ok := m.faults[code]
	return f, ok
}

// MustFault 返回已知故障码的声明（四个码由加载校验保证存在）；码不在 model.FaultCodes 内即编程错误，panic。
func (m *FaultMatrix) MustFault(code model.FaultCode) Fault {
	f, ok := m.faults[code]
	if !ok {
		panic(fmt.Sprintf("policy: fault %q not in matrix", code))
	}
	return f
}

// ForRefuseReason 返回以该拒答原因收尾的故障（LLM_UNAVAILABLE / DATA_MISSING / KB_MISS）；其余拒答原因 false。
func (m *FaultMatrix) ForRefuseReason(reason string) (Fault, bool) {
	code, ok := m.byReason[reason]
	if !ok {
		return Fault{}, false
	}
	return m.faults[code], true
}

// ForToolError 返回 MCP 错误码归入的故障（TIMEOUT / UPSTREAM → MCP_TIMEOUT）；其余错误码 false。
func (m *FaultMatrix) ForToolError(code string) (Fault, bool) {
	fc, ok := m.byTool[code]
	if !ok {
		return Fault{}, false
	}
	return m.faults[fc], true
}

// Faults 按 model.FaultCodes 顺序返回全部声明（启动日志 / 演练清单用）。
func (m *FaultMatrix) Faults() []Fault {
	out := make([]Fault, 0, len(m.faults))
	for _, c := range model.FaultCodes() {
		out = append(out, m.faults[c])
	}
	return out
}
