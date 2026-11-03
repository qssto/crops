package app

import (
	"testing"

	"agri-steward/internal/config"
)

// 仓库 configs/app.yaml 必须为每个 raw_object.kind 配置保留天数（缺 kind 启动失败）。
func TestRepoRetentionCoversAllKinds(t *testing.T) {
	c, err := config.Load("../../configs/app.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Retention.RequireKinds(RawKindNames()); err != nil {
		t.Fatal(err)
	}
	// 必败：漏一个 kind 即报错。
	days := c.Retention.RawDays()
	delete(days, "EXPORT")
	if err := (config.Retention{RawObjectDays: days}).RequireKinds(RawKindNames()); err == nil {
		t.Fatal("missing EXPORT must fail")
	}
}

// 未启用 L7 时扫描依赖为无类型 nil，路由与处理器为空。
func TestAuditL7Disabled(t *testing.T) {
	a := &App{}
	if a.anchorTicker() != nil || a.retentionTicker() != nil || a.auditL7Handlers() != nil || a.auditL7Mounts() != nil {
		t.Fatal("disabled L7 must expose nothing")
	}
	a.L7 = &AuditL7{}
	if a.anchorTicker() != nil || a.retentionTicker() != nil {
		t.Fatal("typed nil ticker leaked")
	}
}
