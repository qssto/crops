// Package sample 定义评测样本格式（评测设计方案 §5，按 P0 实际需要裁剪），
// 负责加载 eval/suites/**.yaml 与静态校验。格式说明见 eval/README.md。
package sample

import (
	"fmt"
	"strings"

	"agri-steward/internal/model"
)

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
	// Quarantine 是隔离理由（P1-L4 审查修复）：声明即 Runner 不运行该样本，报告单列「隔离样本」，
	// 不计入 N / 通过 / INFRA_ERROR，Gate 0 与红线统计也不含它；仍参与静态校验。写了就必须是非空理由（RuleQuarantine）。
	Quarantine *string `yaml:"quarantine,omitempty"`

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
	Device   *DeviceFixture           `yaml:"device,omitempty"` // P1 L2：RecordingAdapter 的回执序列
	// LocalOrders 是本地库同地块近期 REAL 作业单（P1 L3 重复作业 / 安全间隔期约束的本地来源）；
	// 声明时替换 trial 库读取（env.FixtureLocalOrders），不声明时读 trial 库。
	LocalOrders []LocalOrderFixture `yaml:"local_orders,omitempty"`
	// Dispatch 声明审批脚本之后的下发步骤（P1-L4-A）：声明即跳过 ScriptApprover 的内联复检，改由排空 outbox 走真实下发器。
	Dispatch *DispatchFixture `yaml:"dispatch,omitempty"`
}

// DispatchFixture 是下发步骤（按序执行，每步之后都排空 outbox）：
//  1. 排空 outbox（order.dispatch 走下发器 ①-⑦，notify.send 走占位处理器）；
//  2. Reapprove 非空时对复检退回后的新票（ticket_seq > 1、OPEN）执行该审批脚本（取值同 approver）；
//  3. Redeliver 次：把该单已 DONE 的 order.dispatch 消息重新置 PENDING（模拟重复投递 / 租约过期后另一 worker 取到同一消息）。
type DispatchFixture struct {
	Reapprove string `yaml:"reapprove,omitempty"`
	Redeliver int    `yaml:"redeliver,omitempty"`
}

// DeviceFixture 是样本声明的设备回执序列：RecordingAdapter 的 Dispatch / QueryStatus / Cancel 按调用次序逐个返回；
// 用完后 Dispatch 回 ACCEPTED、QueryStatus 重复最后一条、Cancel 回 CANCELED。
type DeviceFixture struct {
	Receipts []ReceiptFixture `yaml:"receipts"`
	// P1-L4-B：按方法、按作业单的回执 / 错误脚本（优先于 Receipts），与回执投递步骤（见 device_script.go）。
	Scripts []DeviceScript `yaml:"scripts,omitempty"`
	Steps   []ReceiptStep  `yaml:"steps,omitempty"`
}

// ReceiptFixture 是一条标准回执（字段同 model.DeviceReceipt；device_job_id 为空时取 recorded-{order_id}）。
type ReceiptFixture struct {
	Status      string   `yaml:"status"`
	FailureKind string   `yaml:"failure_kind,omitempty"`
	DoneAreaMu  *float64 `yaml:"done_area_mu,omitempty"`
	Code        string   `yaml:"code,omitempty"`
}

// Receipt 转为标准回执（未补 device_job_id，不校验）。
func (f ReceiptFixture) Receipt() model.DeviceReceipt {
	return model.DeviceReceipt{Status: model.AttemptStatus(f.Status), FailureKind: model.FailureKind(f.FailureKind),
		DoneAreaMu: f.DoneAreaMu, Code: f.Code}
}

// LocalOrderFixture 是一条本地作业单：work_type / status 取 model 枚举，updated_at 为 RFC3339，product 写入 payload.product。
type LocalOrderFixture struct {
	WorkType  string `yaml:"work_type"`
	Status    string `yaml:"status"`
	UpdatedAt string `yaml:"updated_at"`
	Product   string `yaml:"product,omitempty"`
}

// ToolFixture 是某个工具的一条桩返回：按 match 子集匹配入参；return / returns / error 三选一：
// return 对每次命中都返回同一值，returns 按调用次序逐个返回（用完后再调用记 unexpected_call），
// error 对每次命中都注入同一故障。
type ToolFixture struct {
	Match   map[string]any `yaml:"match,omitempty"`
	Return  any            `yaml:"return,omitempty"`
	Returns []any          `yaml:"returns,omitempty"`
	Error   string         `yaml:"error,omitempty"`
}

// 故障注入类型（fixtures.mcp.*.error），FixtureMCP 映射为 ToolResult.Status / ErrorCode。
const (
	FaultTimeout   = "timeout"
	FaultEmpty     = "empty"
	FaultMalformed = "malformed"
)

// MatchKeyField 是 match 中的地块合成编码键（field: field-003），FixtureMCP 装配时解析为 field_id。
const MatchKeyField = "field"

// Problem 返回 fixture 的结构问题，空串表示合法：return / returns / error 必须恰好写一个，returns 不得含空值。
func (f ToolFixture) Problem() string {
	set := 0
	for _, has := range []bool{f.Return != nil, len(f.Returns) > 0, f.Error != ""} {
		if has {
			set++
		}
	}
	if set != 1 {
		return "exactly one of return / returns / error is required"
	}
	for i, r := range f.Returns {
		if r == nil {
			return fmt.Sprintf("returns[%d] is null", i)
		}
	}
	return ""
}

// Expect 是期望结果。
type Expect struct {
	Outcome      model.Outcome    `yaml:"outcome"`
	RefuseReason string           `yaml:"refuse_reason,omitempty"`
	Advice       *AdviceExpect    `yaml:"advice,omitempty"`
	WorkOrder    *WorkOrderExpect `yaml:"work_order,omitempty"`
	Trajectory   Trajectory       `yaml:"trajectory,omitempty"`
	PII          []PIIExpect      `yaml:"pii,omitempty"`
	// Foreign 是问题中出现的非本农场标识（农场 / 地块编码、设备 ID），须原样出现在 question 中；
	// cross_farm_isolation 断言其不出现在输出与工具入参（P1 L1-B 红队样本）。
	Foreign []string `yaml:"foreign,omitempty"`
	// Constraint 断言开票前约束拦截（P1 L3，constraint_blocked 使用）：rule_ids 须全部出现在拦截结果中。
	// 不写表示不应被拦截。
	Constraint *ConstraintExpect `yaml:"constraint,omitempty"`
	// Dispatch 断言下发侧结果（P1-L4-A，recheck_returned 使用；须同时声明 fixtures.dispatch）。
	Dispatch *DispatchExpect `yaml:"dispatch,omitempty"`
	// WorkDone 断言作业完成回写（P1-L4-B，partially_done 使用）；不写表示不检查。
	WorkDone *WorkDoneExpect `yaml:"work_done,omitempty"`
	// Approval 断言审批闸门与定级（P1 L5，approval_gate 使用，见 approval.go）；不写表示不检查。
	Approval *ApprovalExpect `yaml:"approval,omitempty"`
}

// DispatchExpect 是下发侧期望：RecheckReturned 为 true 时断言下发前复检不通过、单退回 PENDING_APPROVAL(RECHECK_FAILED)、
// 新票 OPEN、notify.send 已入 outbox；为 false 时断言没有退回。DeviceDispatches 非空时断言设备下发调用（DryRun / Dispatch）次数。
type DispatchExpect struct {
	RecheckReturned  bool `yaml:"recheck_returned"`
	DeviceDispatches *int `yaml:"device_dispatches,omitempty"`
}

// ConstraintExpect 是约束拦截期望：run 带 CONSTRAINT_BLOCKED、无票据、作业单留 DRAFT、设备零调用。
type ConstraintExpect struct {
	RuleIDs []string `yaml:"rule_ids"`
}

// PIIExpect 声明问题中出现的一处敏感信息：L5 打分器断言其不出现在日志、
// 审计与输出中（P0 §6.6 脱敏）。Value 须原样出现在 question 中。
type PIIExpect struct {
	Kind  string `yaml:"kind"`
	Value string `yaml:"value"`
}

// AdviceExpect 是 outcome=ADVICE 时对 AdviceCard 的期望。
type AdviceExpect struct {
	Category   string         `yaml:"category"`
	AdviceType string         `yaml:"advice_type"`
	Conclusion string         `yaml:"conclusion"`
	MustCite   []string       `yaml:"must_cite"`
	Target     string         `yaml:"target,omitempty"`     // 防治对象 id（登记表 targets），product_in 非空时必填（L3）
	ProductIn  []string       `yaml:"product_in,omitempty"` // ⊆ calc.Candidates(crop, target, region)
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

// QuarantineReason 返回隔离理由；未声明 quarantine 时 ok 为 false。
func (s *Sample) QuarantineReason() (reason string, ok bool) {
	if s.Quarantine == nil {
		return "", false
	}
	return strings.TrimSpace(*s.Quarantine), true
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
