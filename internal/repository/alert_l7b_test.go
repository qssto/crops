package repository

import (
	"strings"
	"testing"
)

// 审查修复 P3：无引用扫描定版 SQL 排除 REFUSED（拒答不命中）、无结尾分号、按 (tenant_id, farm_id, advice_id) 关联引用。
// 行为层面（REFUSED / 有引用 / 窗口外不命中）见 TestAlertRepo_AdviceWithoutCitation_Integration。
func TestAdviceWithoutCitationSQL(t *testing.T) {
	sql := strings.TrimSpace(adviceWithoutCitationSQL)
	tests := []struct {
		name string
		ok   bool
	}{
		{name: "REFUSED excluded", ok: strings.Contains(sql, "a.outcome<>'REFUSED'")},
		{name: "no trailing semicolon", ok: !strings.HasSuffix(sql, ";")},
		{name: "citation joined by scope", ok: strings.Contains(sql,
			"c.tenant_id=a.tenant_id AND c.farm_id=a.farm_id AND c.advice_id=a.id")},
		{name: "window bounded", ok: strings.Contains(sql, "a.created_at BETWEEN ? AND ?")},
	}
	for _, tt := range tests {
		if !tt.ok {
			t.Errorf("%s: sql = %s", tt.name, sql)
		}
	}
}
