package model

import (
	"slices"
	"testing"
)

func TestSensitiveAudit(t *testing.T) {
	cases := []struct {
		action, result string
		want           AuditAction
	}{
		{string(AuditFlagChange), AuditResultOK, AuditSensitiveFlagChange},
		{string(AuditOrderCancel), AuditResultOK, AuditSensitiveOrderCancel},
		{string(AuditRawRead), AuditResultOK, AuditSensitiveRawRead},
		{string(AuditExport), AuditResultOK, AuditSensitiveExport},
		{string(AuditRawRead), AuditResultDenied, ""}, // 拒绝不留二次痕
		{"ASK", AuditResultOK, ""},
		{string(AuditSensitiveRawRead), AuditResultOK, ""}, // 二次留痕本身不再递归
	}
	for _, c := range cases {
		got, ok := SensitiveAudit(c.action, c.result)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("SensitiveAudit(%s, %s) = %q %v, want %q", c.action, c.result, got, ok, c.want)
		}
	}
}

func TestRawKinds(t *testing.T) {
	ks := RawKinds()
	if !slices.Contains(ks, RawExport) || !slices.Contains(ks, RawLLMIO) {
		t.Fatalf("kinds %v", ks)
	}
	seen := map[RawKind]bool{}
	for _, k := range ks {
		if seen[k] {
			t.Fatalf("duplicate kind %s", k)
		}
		seen[k] = true
	}
}
