package sample

import (
	"strings"
	"testing"
)

func strp(v string) *string { return &v }

// TestCheckQuarantine（P1-L4 审查修复）：quarantine 声明了就必须带非空理由。
// 应失败：空串 / 纯空白理由被放过，隔离样本无从追溯。
func TestCheckQuarantine(t *testing.T) {
	tests := []struct {
		name    string
		reason  *string
		wantErr bool
	}{
		{name: "not quarantined"},
		{name: "with reason", reason: strp("需 REAL 评测设施，见 dev-log P1-L4")},
		{name: "empty reason", reason: strp(""), wantErr: true},
		{name: "blank reason", reason: strp("  \t"), wantErr: true},
	}
	v := NewValidator(DefaultRules(mustTaxonomy(t), repoKB(t))...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := validSet()
			set[1].Quarantine = tt.reason
			report := v.Validate(set)
			var hit bool
			for _, viol := range report.Violations {
				if viol.Rule == RuleQuarantine && viol.Subject() == "s-1" && strings.Contains(viol.Msg, "non-empty reason") {
					hit = true
				}
			}
			if hit != tt.wantErr || (!tt.wantErr && !report.OK()) {
				t.Fatalf("violations = %v, want quarantine error %v", report.Violations, tt.wantErr)
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
