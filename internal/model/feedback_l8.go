package model

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// P2 L8 农户采纳 / 差评（advice_feedback，§6.9 技术实现 ① 1）。

// FeedbackVerdict 是反馈结论。
type FeedbackVerdict string

// 反馈结论（advice_feedback.verdict varchar(8)）。
const (
	FeedbackAdopt   FeedbackVerdict = "ADOPT"
	FeedbackDislike FeedbackVerdict = "DISLIKE"
)

// MaxFeedbackRequestID 是 advice_feedback.request_id 的长度上限（varchar(64)）。
const MaxFeedbackRequestID = 64

// ErrFeedbackRequestConflict 表示 request_id 已被别的农场 / 租户、或同场他人 / 另一建议卡用过（uk_request 全局唯一）：
// 不回首次的行，按冲突处理，各种不一致同一错误。
var ErrFeedbackRequestConflict = errors.New("feedback request_id already used")

// AdviceFeedback 是 advice_feedback 一行（只追加；request_id 幂等）。Comment 已经 pii_mask。
type AdviceFeedback struct {
	ID        uint64            `json:"id"`
	TenantID  uint64            `json:"tenant_id"`
	FarmID    uint64            `json:"farm_id"`
	AdviceID  uint64            `json:"advice_id"`
	RunID     uint64            `json:"run_id"`
	UserID    uint64            `json:"user_id"`
	Verdict   FeedbackVerdict   `json:"verdict"`
	Category  *RejectReasonCode `json:"category,omitempty"`
	Comment   *string           `json:"comment,omitempty"`
	RequestID string            `json:"request_id"`
	CreatedAt time.Time         `json:"created_at"`
}

// ValidateFeedback 校验反馈：DISLIKE 的 category 与 comment 照驳回原因规则（五类之一、OTHER 须带文字、≤512 字符）；
// ADOPT 不带 category，comment 同样 ≤512 字符；request_id 必填且 ≤64 字符。
func ValidateFeedback(verdict FeedbackVerdict, category RejectReasonCode, comment, requestID string) error {
	switch {
	case strings.TrimSpace(requestID) == "":
		return fmt.Errorf("request_id required")
	case len(requestID) > MaxFeedbackRequestID:
		return fmt.Errorf("request_id over %d bytes", MaxFeedbackRequestID)
	}
	switch verdict {
	case FeedbackDislike:
		if err := ValidateRejectReason(category, comment); err != nil {
			return fmt.Errorf("dislike: %w", err)
		}
	case FeedbackAdopt:
		if category != "" {
			return fmt.Errorf("category only allowed for DISLIKE")
		}
		if utf8.RuneCountInString(comment) > MaxRejectReasonText {
			return fmt.Errorf("comment over %d chars", MaxRejectReasonText)
		}
	default:
		return fmt.Errorf("verdict %q, want ADOPT / DISLIKE", verdict)
	}
	return nil
}
