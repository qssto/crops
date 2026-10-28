// Package sample 定义评测样本格式（评测设计方案 §5，按 P0 实际需要裁剪），
// 负责加载 eval/suites/**.yaml 与静态校验。格式说明见 eval/README.md。
package sample

import "agri-steward/internal/model"

// Sample 是一条评测任务：问题 + 情境 + fixture + 期望，带署名。一个文件一个样本。
type Sample struct {
	ID                string   `yaml:"id"`
	Version           int      `yaml:"version"`
	Suite             string   `yaml:"suite"`
	Difficulty        string   `yaml:"difficulty,omitempty"`
	Tags              []string `yaml:"tags"`
	Question          string   `yaml:"question"`
	Context           Context  `yaml:"context"`
	Fixtures          Fixtures `yaml:"fixtures"`
	KnowledgeRef      string   `yaml:"knowledge_ref"`
	Expect            Expect   `yaml:"expect"`
	ReferenceSolution string   `yaml:"reference_solution,omitempty"`
	Author            string   `yaml:"author"`
	ReviewStatus      string   `yaml:"review_status"`
	SignedAt          string   `yaml:"signed_at"`
	Notes             string   `yaml:"notes,omitempty"`

	// Path 是样本文件路径，加载时填写，不来自 YAML。
	Path string `yaml:"-"`
}

// Context 是提问情境。farm / field 是合成编码，L1 起评测播种时写入 field_profile.ext_field_code。
type Context struct {
	Farm  string `yaml:"farm"`
	Field string `yaml:"field"`
	Role  string `yaml:"role"`
	User  string `yaml:"user"`
}

// Fixtures 是本次运行所见的外部世界（L1 FrozenClock、L2 FixtureMCP、L6 记忆与脚本审批人）。
type Fixtures struct {
	Clock    string                   `yaml:"clock"`
	MCP      map[string][]ToolFixture `yaml:"mcp,omitempty"`
	Memory   map[string]any           `yaml:"memory,omitempty"`
	Approver string                   `yaml:"approver,omitempty"`
}

// ToolFixture 是某个工具的一条桩返回：按 match 匹配入参，return 单次返回，
// returns 按调用次序返回，error 注入故障。
type ToolFixture struct {
	Match   map[string]any `yaml:"match,omitempty"`
	Return  any            `yaml:"return,omitempty"`
	Returns []any          `yaml:"returns,omitempty"`
	Error   string         `yaml:"error,omitempty"`
}

// Expect 是期望结果。
type Expect struct {
	Outcome      model.Outcome    `yaml:"outcome"`
	RefuseReason string           `yaml:"refuse_reason,omitempty"`
	Advice       *AdviceExpect    `yaml:"advice,omitempty"`
	WorkOrder    *WorkOrderExpect `yaml:"work_order,omitempty"`
	Trajectory   Trajectory       `yaml:"trajectory,omitempty"`
}

// AdviceExpect 是 outcome=ADVICE 时对 AdviceCard 的期望。
type AdviceExpect struct {
	Category   string         `yaml:"category"`
	AdviceType string         `yaml:"advice_type"`
	Conclusion string         `yaml:"conclusion"`
	MustCite   []string       `yaml:"must_cite"`
	ProductIn  []string       `yaml:"product_in,omitempty"`
	Numbers    []NumberExpect `yaml:"numbers,omitempty"`
}

// NumberExpect 是一个数值字段的期望；Value 为空表示待 L3 calc 回填。
type NumberExpect struct {
	Field     string   `yaml:"field"`
	Value     *float64 `yaml:"value"`
	Unit      string   `yaml:"unit"`
	Precision float64  `yaml:"precision"`
}

// WorkOrderExpect 是作业单期望终态。
type WorkOrderExpect struct {
	FinalStatus string `yaml:"final_status"`
}

// Trajectory 是轨迹断言（L4 / L5 打分器使用）。
type Trajectory struct {
	MustCall       []string `yaml:"must_call,omitempty"`
	MustNotCall    []string `yaml:"must_not_call,omitempty"`
	StopAtApproval bool     `yaml:"stop_at_approval,omitempty"`
	Budget         *Budget  `yaml:"budget,omitempty"`
}

// Budget 是单个 trial 的步数与 token 上限。
type Budget struct {
	MaxSteps  int `yaml:"max_steps,omitempty"`
	MaxTokens int `yaml:"max_tokens,omitempty"`
}

// IsRefusal 判断样本是否为应拒答样本。
func (s *Sample) IsRefusal() bool {
	return s.Expect.Outcome == model.OutcomeRefused
}

// HasAnyTag 判断样本是否带有任一给定 tag；tags 为空视为不过滤。
func (s *Sample) HasAnyTag(tags []string) bool {
	if len(tags) == 0 {
		return true
	}
	for _, want := range tags {
		for _, have := range s.Tags {
			if have == want {
				return true
			}
		}
	}
	return false
}
