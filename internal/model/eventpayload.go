package model

import (
	"encoding/json"
	"time"
)

// 图内事件的 payload 契约（L7 冻结）：callbacks 写入，指标 / run 详情 / transcript 读取。
// 只含 ID 与控制字段；文本类字段写入前须经 pii.Mask。

// RunStartedPayload 是 RUN_STARTED（投影为 ASK）。
type RunStartedPayload struct {
	FieldID  uint64 `json:"field_id,omitempty"`
	Question string `json:"question"` // 已脱敏
	Channel  string `json:"channel,omitempty"`
}

// StepPayload 是 STEP_STARTED / STEP_ENDED；step_id 写在 run_event.step_id 列。
type StepPayload struct {
	Node      string `json:"node"`
	Status    string `json:"status,omitempty"` // STEP_ENDED：OK / ERROR / INTERRUPTED
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"` // 错误码或脱敏后的简述
}

// LLMCallPayload 是 LLM_CALL_END；模型输入输出原文在 run_event.raw_object_id 指向的 LLM_IO 对象。
type LLMCallPayload struct {
	Task             string `json:"task"`
	Model            string `json:"model,omitempty"`
	Status           string `json:"status"` // OK / ERROR
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	LatencyMS        int64  `json:"latency_ms"`
	Error            string `json:"error,omitempty"`
}

// KBLoadedPayload 是 KB_LOADED。P2 L5 只追加字段（audit 按 KBRef 投影，rundetail 原样透传）。
type KBLoadedPayload struct {
	KBRef   string   `json:"kb_ref,omitempty"`
	Entries []string `json:"entries"` // kb_id@version

	// P2 L5 加载埋点：问题类型（一级 taxonomy code）、情境维度、逐条选入原因与名次、策略版本、run 内第几次加载。
	QuestionType string          `json:"question_type,omitempty"`
	Situation    *KBSituation    `json:"situation,omitempty"`
	Items        []KBLoadedEntry `json:"items,omitempty"` // 与 Entries 同序
	Strategy     string          `json:"strategy,omitempty"`
	LoadSeq      int             `json:"load_seq"`
}

// KBSituation 是 KB_LOADED 的情境维度（作物 / 生育期 / 区域 / 子类 / 防治对象 / 排序子类；空值省略）。
type KBSituation struct {
	Crop   string `json:"crop"`
	Stage  string `json:"stage,omitempty"`
	Region string `json:"region"`
	Topic  string `json:"topic,omitempty"`
	Target string `json:"target,omitempty"`

	RankTopic string `json:"rank_topic,omitempty"` // 分类器子类：只参与 v1 排序（topic_match），不参与过滤
}

// KBLoadedEntry 是 KB_LOADED 的一条加载结果：kb:<kb_id>@<version>、选入原因（MUST_INCLUDE / RANKED / COMMON）、名次。
type KBLoadedEntry struct {
	Ref        string `json:"ref"`
	PickReason string `json:"pick_reason"`
	Rank       int    `json:"rank"`
}

// AdviceReadyPayload 是 ADVICE_READY（投影为 VALIDATE / OK）。
type AdviceReadyPayload struct {
	AdviceID     uint64 `json:"advice_id,omitempty"`
	Outcome      string `json:"outcome"`
	RefuseReason string `json:"refuse_reason,omitempty"`
}

// RunEndPayload 是 RUN_COMPLETED / RUN_FAILED / RUN_INTERRUPTED。
type RunEndPayload struct {
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	InterruptType string `json:"interrupt_type,omitempty"`
	Resumed       bool   `json:"resumed,omitempty"`
}

// DispatchedPayload 是 DISPATCHED（投影为 DISPATCH，target=order:{order_id}）：recordIntent 时写入（L8 冻结）。
// P0 恒为 dry_run=true、device_touched=false，审计 summary 即此 payload。
type DispatchedPayload struct {
	OrderID       uint64       `json:"order_id"`
	TicketID      uint64       `json:"ticket_id,omitempty"`
	DispatchMode  DispatchMode `json:"dispatch_mode"`
	DryRun        bool         `json:"dry_run"`
	DeviceTouched bool         `json:"device_touched"`
	// Phase：INTENT（⑤ recordIntent）/ RESULT（⑦ recordResult）。
	Phase string `json:"phase"`
}

// DispatchedPayload.Phase 取值。
const (
	DispatchPhaseIntent = "INTENT"
	DispatchPhaseResult = "RESULT"
)

// DryRunDispatchResult 是 dry-run 下 work_order.dispatch_result 的 JSON（L8 冻结）：
// Web 页「dry-run 可视化」与 run 详情直接展示，页面据 device_touched=false 标注「未触达真实设备」。
type DryRunDispatchResult struct {
	DryRun        bool            `json:"dry_run"`
	DeviceTouched bool            `json:"device_touched"`
	Adapter       string          `json:"adapter"`  // 如 mock
	Commands      json.RawMessage `json:"commands"` // MockAdapter.DryRun 返回的将下发指令（数组）
	Params        json.RawMessage `json:"params"`   // 预期作业参数（work_order.payload 摘要：药剂 / 用量 / 面积 / 时间窗）
	SimulatedAt   time.Time       `json:"simulated_at"`
}

// StepPayload.Status / LLMCallPayload.Status 取值（L7-A 追加，只加不改）。
const (
	StepStatusOK          = "OK"
	StepStatusError       = "ERROR"
	StepStatusInterrupted = "INTERRUPTED"
	LLMStatusOK           = "OK"
	LLMStatusError        = "ERROR"
)
