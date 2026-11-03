package sample

import (
	"strings"
	"testing"
)

func strp(v string) *string { return &v }

// TestCheckQuarantine（P1-L4 审查修复；P1-L8-A 审查修复补 redteam）：quarantine 声明了就必须带非空理由，且 redteam 不得隔离。
// 应失败：空串 / 纯空白理由被放过，隔离样本无从追溯；redteam 样本带理由隔离被放过，红线夜间失去覆盖。
func TestCheckQuarantine(t *testing.T) {
	tests := []struct {
		name    string
		suite   string
		reason  *string
		wantMsg string // 空表示应通过
	}{
		{name: "not quarantined"},
		{name: "with reason", reason: strp("需 REAL 评测设施，见 dev-log P1-L4")},
		{name: "empty reason", reason: strp(""), wantMsg: "non-empty reason"},
		{name: "blank reason", reason: strp("  \t"), wantMsg: "non-empty reason"},
		{name: "redteam not quarantined", suite: "redteam"},
		{name: "redteam quarantined", suite: "redteam", reason: strp("暂时跑不过"), wantMsg: "redteam samples must not be quarantined"},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := validSet()
			if tt.suite != "" {
				for _, s := range set {
					s.Suite = tt.suite
				}
			}
			set[1].Quarantine = tt.reason
			report := v.Validate(set)
			var hit bool
			for _, viol := range report.Violations {
				if viol.Rule == RuleQuarantine && viol.Subject() == "s-1" && strings.Contains(viol.Msg, tt.wantMsg) {
					hit = true
				}
			}
			if tt.wantMsg == "" {
				if !report.OK() {
					t.Fatalf("violations = %v, want none", report.Violations)
				}
				return
			}
			if !hit {
				t.Fatalf("violations = %v, want quarantine error containing %q", report.Violations, tt.wantMsg)
			}
		})
	}
}

// TestQuarantineReason：YAML 里 `quarantine: ""` 与缺省可区分（指针字段），理由去首尾空白。
func TestQuarantineReason(t *testing.T) {
	var s Sample
	if _, ok := s.QuarantineReason(); ok {
		t.Fatal("unset quarantine reported as quarantined")
	}
	s.Quarantine = strp("  需 REAL 评测设施 ")
	if r, ok := s.QuarantineReason(); !ok || r != "需 REAL 评测设施" {
		t.Fatalf("reason = %q, %v", r, ok)
	}
}
