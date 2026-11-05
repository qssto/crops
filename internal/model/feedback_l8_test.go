package model

import (
	"strings"
	"testing"
)

// P2 L8 功能任务 1：「其他」驳回须填文字不回退（审批与差评共用 ValidateRejectReason）。
func TestValidateRejectReason_OtherNeedsText(t *testing.T) {
	for _, text := range []string{"", " \t　"} {
		if err := ValidateRejectReason(RejectOther, text); err == nil {
			t.Errorf("OTHER with text %q accepted, want error", text)
		}
	}
	if err := ValidateRejectReason(RejectOther, "药剂与上次相同"); err != nil {
		t.Errorf("OTHER with text: %v", err)
	}
}

// 应失败：DISLIKE 缺 category / 非五类 / OTHER 无文字；ADOPT 带 category；verdict 非法；request_id 缺失或超长；comment 超长。
func TestValidateFeedback(t *testing.T) {
	long := strings.Repeat("字", MaxRejectReasonText+1)
	tests := []struct {
		name     string
		verdict  FeedbackVerdict
		category RejectReasonCode
		comment  string
		reqID    string
		ok       bool
	}{
		{"adopt", FeedbackAdopt, "", "", "r1", true},
		{"adopt with comment", FeedbackAdopt, "", "好用", "r1", true},
		{"dislike dosage", FeedbackDislike, RejectDosage, "", "r1", true},
		{"dislike other with text", FeedbackDislike, RejectOther, "太贵", "r1", true},
		{"dislike without category", FeedbackDislike, "", "", "r1", false},
		{"dislike unknown category", FeedbackDislike, "WEATHER", "", "r1", false},
		{"dislike other without text", FeedbackDislike, RejectOther, "  ", "r1", false},
		{"adopt with category", FeedbackAdopt, RejectDosage, "", "r1", false},
		{"bad verdict", "LIKE", "", "", "r1", false},
		{"empty verdict", "", "", "", "r1", false},
		{"missing request_id", FeedbackAdopt, "", "", " ", false},
		{"request_id too long", FeedbackAdopt, "", "", strings.Repeat("a", MaxFeedbackRequestID+1), false},
		{"adopt comment too long", FeedbackAdopt, "", long, "r1", false},
		{"dislike comment too long", FeedbackDislike, RejectTiming, long, "r1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFeedback(tt.verdict, tt.category, tt.comment, tt.reqID)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateFeedback = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}
