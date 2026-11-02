package model

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// P1 L5 审批体系的领域常量（§6.6）：风险原因、驳回原因分类、驳回记录行。

// 风险原因（approval_ticket.risk_reasons 的取值，按命中顺序写入 JSON 数组）。
const (
	RiskReasonPesticide = "PESTICIDE"  // 用药类（SPRAY）
	RiskReasonLargeArea = "LARGE_AREA" // 面积达到 risk_matrix.yaml 的 large_area_mu
	// RiskReasonLargeAreaUnknown 是作业面积未知（payload.area_mu 缺失 / 非正 / 非有限数）：无从核对是否大面积，按 HIGH（失败关闭）。
	RiskReasonLargeAreaUnknown = "LARGE_AREA:unknown"
	// RiskReasonNearThresholdPrefix 后接 rule_id，如 NEAR_THRESHOLD:wind_speed_max（PRE_APPROVAL 通过但离阈值在裕度内）。
	RiskReasonNearThresholdPrefix = "NEAR_THRESHOLD:"
)

// NearThresholdReason 返回 NEAR_THRESHOLD:{metric}。
func NearThresholdReason(metric string) string { return RiskReasonNearThresholdPrefix + metric }

// IsPesticide 判断作业类型是否为用药类（审批须含 agronomist，风险原因 PESTICIDE）。
func IsPesticide(wt WorkType) bool { return wt == WorkSpray }

// RejectReasonCode 是驳回原因分类（rejection_reason.reason_code）。
type RejectReasonCode string

const (
	RejectDosage   RejectReasonCode = "DOSAGE"   // 用量不对
	RejectTiming   RejectReasonCode = "TIMING"   // 时机不对
	RejectProduct  RejectReasonCode = "PRODUCT"  // 药剂不对
	RejectEvidence RejectReasonCode = "EVIDENCE" // 依据不足
	RejectOther    RejectReasonCode = "OTHER"    // 其他（须带 reason_text）
)

// RejectReasonCodes 是全部驳回原因分类（校验与文档用）。
var RejectReasonCodes = []RejectReasonCode{RejectDosage, RejectTiming, RejectProduct, RejectEvidence, RejectOther}

// MaxRejectReasonText 是 rejection_reason.reason_text 的字符上限（varchar(512)）。
const MaxRejectReasonText = 512

// ValidateRejectReason 校验驳回原因：code 必须是五类之一；OTHER 时 text 非空；text 不超过 512 字符。
func ValidateRejectReason(code RejectReasonCode, text string) error {
	switch {
	case code == "":
		return fmt.Errorf("reason_code required for REJECT")
	case !slices.Contains(RejectReasonCodes, code):
		return fmt.Errorf("reason_code %q, want one of DOSAGE / TIMING / PRODUCT / EVIDENCE / OTHER", code)
	case code == RejectOther && strings.TrimSpace(text) == "":
		return fmt.Errorf("reason_text required when reason_code is OTHER")
	case utf8.RuneCountInString(text) > MaxRejectReasonText:
		return fmt.Errorf("reason_text over %d chars", MaxRejectReasonText)
	}
	return nil
}

// RejectionReason 是 rejection_reason 一行（只追加）；OrderID 在导出时由 approval_ticket 连表取得。
type RejectionReason struct {
	ID           uint64           `json:"id"`
	TenantID     uint64           `json:"tenant_id"`
	FarmID       uint64           `json:"farm_id"`
	TicketID     uint64           `json:"ticket_id"`
	OrderID      uint64           `json:"order_id"`
	AdviceID     uint64           `json:"advice_id"`
	RunID        uint64           `json:"run_id"`
	ReasonCode   RejectReasonCode `json:"reason_code"`
	ReasonText   *string          `json:"reason_text,omitempty"`
	OperatorID   uint64           `json:"operator_id"`
	OperatorRole Role             `json:"operator_role"`
	CreatedAt    time.Time        `json:"created_at"`
}
