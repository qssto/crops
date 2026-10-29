//go:build integration

package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

type itPresign struct{}

func (itPresign) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://minio.it/" + key + "?sig=1", nil
}

// 验收 5：RAW_READ 授权读写 OK 审计（与签发同事务，带 raw_object_id），越权 / 跨 run 写 DENIED 并返回 403。
func TestAuditService_ReadRaw_Integration(t *testing.T) {
	e := itest.Setup(t, "svcl7")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	otherRun := itest.InsertRun(t, slot.DB, t0)
	if err := slot.DB.WithContext(t.Context()).Exec(`INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key,
  sha256, size_bytes, created_at) VALUES (?, ?, ?, 'LLM_IO', 'raw/it/llm-1', REPEAT('a', 64), 10, ?)`,
		itest.TenantID, itest.FarmID, runID, t0).Error; err != nil {
		t.Fatal(err)
	}
	rawID := uint64(itest.Count(t, slot.DB, "SELECT MAX(id) FROM raw_object"))
	svc, err := service.NewAuditService(service.AuditDeps{Store: repository.RawAccessRepo{DB: slot.DB},
		Presign: itPresign{}, Clock: slot.Clock, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	agronomist := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001,
		Roles: []model.Role{model.RoleAgronomist}}
	operator := agronomist
	operator.UserID, operator.Roles = 1002, []model.Role{model.RoleOperator}

	tests := []struct {
		name    string
		scope   model.Scope
		runID   uint64
		wantErr error
		result  string
	}{
		{name: "authorized", scope: agronomist, runID: runID, result: model.AuditResultOK},
		{name: "operator", scope: operator, runID: runID, wantErr: service.ErrForbidden, result: model.AuditResultDenied},
		{name: "cross run", scope: agronomist, runID: otherRun, wantErr: service.ErrForbidden, result: model.AuditResultDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, err := svc.ReadRaw(t.Context(), tt.scope, tt.runID, rawID)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if (tt.wantErr == nil) != (link.URL != "") {
				t.Fatalf("link = %+v", link)
			}
			if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action='RAW_READ' AND run_id=?
				AND actor_type='user' AND actor_id=? AND target=? AND result=?`,
				tt.runID, tt.scope.UserID, "raw:"+strconv.FormatUint(rawID, 10), tt.result); n != 1 {
				t.Fatalf("RAW_READ %s audit rows = %d, want 1", tt.result, n)
			}
		})
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action='RAW_READ' AND result='OK'
		AND raw_object_id=? AND JSON_EXTRACT(summary, '$.url') IS NULL`, rawID); n != 1 {
		t.Fatalf("OK audit must link raw_object_id and never store the URL, rows = %d", n)
	}
}
