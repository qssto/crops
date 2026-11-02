package model

import "encoding/json"

// ConstraintBlockedPayload 是 CONSTRAINT_BLOCKED（P1 L3，投影为 APPROVAL / FAILED，target=order:{order_id}）：
// 开票前法规前置条件或约束校验未通过，作业单留 DRAFT、不建票。RuleIDs 为未通过的 rule_id（按校验顺序）。
// constraint_check 的 PRE_APPROVAL 行只保留最近一次复核结果（同 attempt upsert），历次拦截的证据以本事件 Checks 为准
// （run_event 只追加）；整个 payload ≤ 8KB，超出时从 Checks 末尾截掉若干行（仍不够再截 RuleIDs）并置 Truncated。
type ConstraintBlockedPayload struct {
	OrderID   uint64         `json:"order_id"`
	RuleIDs   []string       `json:"rule_ids"`
	Checks    []BlockedCheck `json:"checks"`
	Truncated bool           `json:"truncated,omitempty"`
}

// BlockedCheck 是一条未通过的校验行：observed / threshold 原样取自 constraint_check（threshold.kb 为 kb:<kb_id>@<version>），
// Note 是 observed.note（未通过原因）。
type BlockedCheck struct {
	RuleID    string          `json:"rule_id"`
	Observed  json.RawMessage `json:"observed"`
	Threshold json.RawMessage `json:"threshold"`
	Note      string          `json:"note,omitempty"`
}
