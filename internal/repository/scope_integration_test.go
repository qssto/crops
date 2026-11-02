//go:build integration

package repository_test

import (
	"errors"
	"strconv"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 验收 4（P1 L1-B）：用种子里的第二个农场 (1,2)（区域 安徽，field-201 / field-202，P1-L1-A 加入种子）。
// 越界 field_id 返回 SCOPE_DENIED 并写 CROSS_FARM_DENIED / DENIED 审计；农场区域按 Scope 读取。
func TestFieldScopeGuardAndRegions_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	var own, foreign uint64
	if err := slot.DB.WithContext(t.Context()).Raw("SELECT id FROM field_profile WHERE tenant_id=? AND farm_id=? ORDER BY id LIMIT 1",
		itest.TenantID, itest.FarmID).Scan(&own).Error; err != nil || own == 0 {
		t.Fatalf("seed field: id=%d err=%v", own, err)
	}
	if err := slot.DB.WithContext(t.Context()).Raw("SELECT id FROM field_profile WHERE tenant_id=1 AND farm_id=2 ORDER BY id LIMIT 1").
		Scan(&foreign).Error; err != nil || foreign == 0 {
		t.Fatalf("farm 2 field: id=%d err=%v", foreign, err)
	}
	farmA := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	farmB := model.Scope{TenantID: 1, FarmID: 2, UserID: 2001}
	runID := itest.InsertRun(t, slot.DB, t0)
	g := repository.FieldScopeGuard{DB: slot.DB, Clock: env.NewFrozenClock(t0)}

	tests := []struct {
		name   string
		scope  model.Scope
		field  uint64
		denied bool
	}{
		{"A own field", farmA, own, false},
		{"B own field", farmB, foreign, false},
		{"A reads B field", farmA, foreign, true},
		{"B reads A field", farmB, own, true},
		{"unknown field", farmA, foreign + 100000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := g.CheckField(t.Context(), tt.scope, runID, tt.field)
			if tt.denied != errors.Is(err, agent.ErrScopeDenied) || (!tt.denied && err != nil) {
				t.Fatalf("CheckField = %v, denied want %v", err, tt.denied)
			}
		})
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id=1 AND action=? AND result=?
  AND actor_type='AGENT' AND target LIKE 'field:%'`, string(model.AuditCrossFarmDenied), model.AuditResultDenied); n != 3 {
		t.Fatalf("CROSS_FARM_DENIED audit rows = %d, want 3", n)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id=1 AND farm_id=1 AND run_id=? AND target=?`,
		runID, "field:"+strconv.FormatUint(foreign, 10)); n != 1 {
		t.Fatalf("A→B audit rows = %d, want 1 (scoped to caller farm and run)", n)
	}
	if err := g.Deny(t.Context(), farmA, 0, "device:uav-p100-02"); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id=1 AND farm_id=1 AND run_id IS NULL
  AND target='device:uav-p100-02' AND action=?`, string(model.AuditCrossFarmDenied)); n != 1 {
		t.Fatalf("device deny audit rows = %d, want 1", n)
	}

	regions := repository.FarmRegions{DB: slot.DB}
	for _, tt := range []struct {
		scope model.Scope
		want  string
		err   error
	}{
		{farmA, "江苏", nil},
		{farmB, "安徽", nil},
		{model.Scope{TenantID: 1, FarmID: 3}, "", model.ErrNotFound},
		{model.Scope{TenantID: 2, FarmID: 2}, "", model.ErrNotFound},
	} {
		got, err := regions.FarmRegion(t.Context(), tt.scope)
		if got != tt.want || !errors.Is(err, tt.err) {
			t.Errorf("FarmRegion(%d,%d) = %q, %v; want %q, %v", tt.scope.TenantID, tt.scope.FarmID, got, err, tt.want, tt.err)
		}
	}
}
