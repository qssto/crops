package policy_test

import (
	"errors"
	"strings"
	"testing"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
)

const (
	faultMatrixPath = "../../../../policy/fault_matrix.yaml"
	textsPath       = "../../../../configs/texts/zh-CN.yaml"
)

const goodTexts = `version: 1
locale: zh-CN
texts:
  t.llm: 模型不可用
  t.llm.degraded: "降级：{decision}；请复核"
  t.mcp: 数据缺失
  t.kb: 知识缺失
  t.dev: 设备离线
`

const goodMatrix = `version: 1
faults:
  LLM_UNAVAILABLE:
    behavior: RULE_FALLBACK
    refuse_reason: LLM_UNAVAILABLE
    text_key: t.llm
    degraded_text_key: t.llm.degraded
    alert: {name: a.llm, level: critical}
  MCP_TIMEOUT:
    behavior: CACHE_FALLBACK
    refuse_reason: DATA_MISSING
    tool_error_codes: [UPSTREAM, TIMEOUT]
    text_key: t.mcp
    alert: {name: a.mcp, level: warning}
  KB_MISS:
    behavior: REFUSE
    refuse_reason: KB_MISS
    text_key: t.kb
    alert: {name: a.kb, level: warning}
  DEVICE_OFFLINE:
    behavior: HOLD_AND_NOTIFY
    text_key: t.dev
    alert: {name: a.dev, level: warning}
`

func mustTexts(t *testing.T, s string) *policy.Texts {
	t.Helper()
	tx, err := policy.ParseTexts([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// 仓库内的矩阵与文案表能加载，四个码齐全，查询入口按矩阵返回。
func TestLoadFaultMatrix_Repo(t *testing.T) {
	m, err := policy.LoadFaultMatrix(faultMatrixPath, textsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(m.Faults()); got != len(model.FaultCodes()) {
		t.Fatalf("faults = %d", got)
	}
	for _, f := range m.Faults() {
		if f.Text == "" || f.Alert.Name == "" || f.Alert.Level == "" {
			t.Errorf("%s incomplete: %+v", f.Code, f)
		}
	}
	for code, want := range map[string]model.FaultCode{"TIMEOUT": model.FaultMCPTimeout, "UPSTREAM": model.FaultMCPTimeout} {
		if f, ok := m.ForToolError(code); !ok || f.Code != want {
			t.Errorf("ForToolError(%s) = %v, %v", code, f.Code, ok)
		}
	}
	for _, code := range []string{"EMPTY", "MALFORMED", "REJECTED", "SCOPE_DENIED"} {
		if _, ok := m.ForToolError(code); ok {
			t.Errorf("ForToolError(%s) should not map to a fault", code)
		}
	}
	for reason, want := range map[string]model.FaultCode{"LLM_UNAVAILABLE": model.FaultLLMUnavailable,
		"DATA_MISSING": model.FaultMCPTimeout, "KB_MISS": model.FaultKBMiss} {
		if f, ok := m.ForRefuseReason(reason); !ok || f.Code != want {
			t.Errorf("ForRefuseReason(%s) = %v, %v", reason, f.Code, ok)
		}
	}
	if _, ok := m.ForRefuseReason("VALIDATION_FAILED"); ok {
		t.Error("VALIDATION_FAILED is not a fault")
	}
	llm := m.MustFault(model.FaultLLMUnavailable)
	if got := llm.Degraded("建议观察"); !strings.Contains(got, "建议观察") || strings.Contains(got, policy.DecisionPlaceholder) {
		t.Errorf("degraded text = %q", got)
	}
}

// 缺码 / 缺文案 / 缺告警 / 行为与实现不一致都启动失败（应失败：任何一项被放过）。
func TestParseFaultMatrix_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		matrix string
		texts  string
		want   string
	}{
		{name: "missing code", matrix: cut(goodMatrix, "  DEVICE_OFFLINE:", 4), want: "DEVICE_OFFLINE: missing"},
		{name: "unknown code", matrix: goodMatrix + "  DISK_FULL:\n    behavior: REFUSE\n", want: `unknown fault "DISK_FULL"`},
		{name: "missing text key", matrix: strings.Replace(goodMatrix, "    text_key: t.kb\n", "", 1),
			want: "KB_MISS: text_key is required"},
		{name: "text not in texts file", texts: strings.Replace(goodTexts, "  t.dev: 设备离线\n", "", 1),
			want: `DEVICE_OFFLINE: text_key "t.dev" has no zh-CN text`},
		{name: "missing degraded text key", matrix: strings.Replace(goodMatrix, "    degraded_text_key: t.llm.degraded\n", "", 1),
			want: "LLM_UNAVAILABLE: degraded_text_key is required"},
		{name: "degraded template without placeholder", texts: strings.Replace(goodTexts, "{decision}", "", 1),
			want: "must contain {decision} exactly once"},
		{name: "text with digits", texts: strings.Replace(goodTexts, "知识缺失", "知识缺失请拨打12316", 1),
			want: "must not contain digits"},
		{name: "missing alert", matrix: strings.Replace(goodMatrix, "    alert: {name: a.kb, level: warning}\n", "", 1),
			want: "KB_MISS: alert.name is required"},
		{name: "bad alert level", matrix: strings.Replace(goodMatrix, "{name: a.dev, level: warning}", "{name: a.dev, level: info}", 1),
			want: `alert.level "info"`},
		{name: "duplicate alert", matrix: strings.Replace(goodMatrix, "name: a.dev", "name: a.kb", 1),
			want: `alert "a.kb" already used by KB_MISS`},
		{name: "duplicate text key", matrix: strings.Replace(goodMatrix, "text_key: t.dev", "text_key: t.kb", 1),
			want: `"t.kb" already used by KB_MISS`},
		{name: "behavior drift", matrix: strings.Replace(goodMatrix, "behavior: CACHE_FALLBACK", "behavior: REFUSE", 1),
			want: "MCP_TIMEOUT: behavior"},
		{name: "refuse reason drift", matrix: strings.Replace(goodMatrix, "refuse_reason: KB_MISS", "refuse_reason: DATA_MISSING", 1),
			want: "KB_MISS: refuse_reason"},
		{name: "upstream not mapped", matrix: strings.Replace(goodMatrix, "[UPSTREAM, TIMEOUT]", "[TIMEOUT]", 1),
			want: "MCP_TIMEOUT: tool_error_codes"},
		{name: "unknown yaml field", matrix: strings.Replace(goodMatrix, "    text_key: t.kb\n", "    text_key: t.kb\n    texts: x\n", 1),
			want: "decode"},
		{name: "bad version", matrix: strings.Replace(goodMatrix, "version: 1", "version: 2", 1), want: "version 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matrix, texts := tt.matrix, tt.texts
			if matrix == "" {
				matrix = goodMatrix
			}
			if texts == "" {
				texts = goodTexts
			}
			_, err := policy.ParseFaultMatrix([]byte(matrix), mustTexts(t, texts))
			if !errors.Is(err, policy.ErrBadFaultMatrix) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want ErrBadFaultMatrix containing %q", err, tt.want)
			}
		})
	}
	if _, err := policy.ParseFaultMatrix([]byte(goodMatrix), mustTexts(t, goodTexts)); err != nil {
		t.Fatalf("good matrix: %v", err)
	}
	if _, err := policy.ParseFaultMatrix([]byte(goodMatrix), nil); !errors.Is(err, policy.ErrBadFaultMatrix) {
		t.Errorf("nil texts err = %v", err)
	}
}

// 文案表与矩阵文件缺失 / 非法同样启动失败。
func TestLoadFaultMatrix_Files(t *testing.T) {
	if _, err := policy.LoadFaultMatrix(faultMatrixPath, "nope.yaml"); err == nil {
		t.Error("missing texts file accepted")
	}
	if _, err := policy.LoadFaultMatrix("nope.yaml", textsPath); err == nil {
		t.Error("missing matrix file accepted")
	}
	for name, s := range map[string]string{"no locale": "version: 1\ntexts: {a: b}\n", "empty": "version: 1\nlocale: zh-CN\n",
		"empty text": "version: 1\nlocale: zh-CN\ntexts: {a: ''}\n", "version": "version: 3\nlocale: zh-CN\ntexts: {a: b}\n"} {
		if _, err := policy.ParseTexts([]byte(s)); !errors.Is(err, policy.ErrBadTexts) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// cut 删除从 marker 行起的 n 行。
func cut(s, marker string, n int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == marker {
			return strings.Join(append(lines[:i:i], lines[min(i+n, len(lines)):]...), "\n")
		}
	}
	return s
}
