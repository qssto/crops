package model

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

// KBLoadedPayload 是 KB_LOADED。
type KBLoadedPayload struct {
	KBRef   string   `json:"kb_ref,omitempty"`
	Entries []string `json:"entries"` // kb_id@version
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

// StepPayload.Status / LLMCallPayload.Status 取值（L7-A 追加，只加不改）。
const (
	StepStatusOK          = "OK"
	StepStatusError       = "ERROR"
	StepStatusInterrupted = "INTERRUPTED"
	LLMStatusOK           = "OK"
	LLMStatusError        = "ERROR"
)
