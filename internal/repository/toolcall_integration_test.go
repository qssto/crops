//go:build integration

package repository_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

func TestToolCallRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	repo := repository.NewToolCallRepo(slot.DB)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	row := func(key, status string, epoch int) model.ToolCallRow {
		return model.ToolCallRow{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, StepID: "enrich",
			CallKey: key, Epoch: epoch, ToolName: "weather.now", PolicyLevel: "read", ArgsDigest: "d", Status: status,
			TimeSensitive: true, ResultSummary: json.RawMessage(`{"v":1}`), CreatedAt: t0.Add(time.Duration(epoch) * time.Minute)}
	}

	failed := row("k0", model.ToolStatusTimeout, 0)
	failed.ErrorCode = "TIMEOUT"
	if _, err := repo.Save(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.FindOK(t.Context(), scope, "k0"); err != nil || ok {
		t.Fatalf("FindOK after failure = %v, %v; want miss", ok, err)
	}
	okID, err := repo.Save(t.Context(), row("k0", model.ToolStatusOK, 0))
	if err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM tool_call WHERE call_key='k0' AND attempt=2 AND id=?", okID); n != 1 {
		t.Fatalf("second save attempt rows = %d, want attempt 2", n)
	}
	hit, ok, err := repo.FindOK(t.Context(), scope, "k0")
	// MySQL JSON 列会规范化空白（{"v": 1}），按 JSON 语义比较
	var summary struct{ V int }
	if err != nil || !ok || hit.ID != okID || json.Unmarshal(hit.ResultSummary, &summary) != nil || summary.V != 1 {
		t.Fatalf("FindOK = %+v, %v, %v; want id %d", hit, ok, err, okID)
	}
	if _, ok, err := repo.FindOK(t.Context(), model.Scope{TenantID: itest.TenantID, FarmID: 2}, "k0"); err != nil || ok {
		t.Fatalf("FindOK other farm = %v, %v; want miss", ok, err)
	}

	// 新 epoch 的 OK 结果作废同 run、同工具的旧 epoch。
	if _, err := repo.Save(t.Context(), row("k1", model.ToolStatusOK, 1)); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM tool_call WHERE run_id=? AND epoch=0 AND superseded_at IS NOT NULL", runID); n != 2 {
		t.Fatalf("superseded epoch-0 rows = %d, want 2", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM tool_call WHERE run_id=? AND epoch=1 AND superseded_at IS NULL", runID); n != 1 {
		t.Fatalf("live epoch-1 rows = %d, want 1", n)
	}
}

func TestFieldScope_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	var fieldID uint64
	if err := slot.DB.WithContext(t.Context()).Raw("SELECT id FROM field_profile WHERE tenant_id=? AND farm_id=? ORDER BY id LIMIT 1",
		itest.TenantID, itest.FarmID).Scan(&fieldID).Error; err != nil || fieldID == 0 {
		t.Fatalf("seed field: id=%d err=%v", fieldID, err)
	}
	fs := repository.FieldScope{DB: slot.DB}
	tests := []struct {
		name  string
		scope model.Scope
		field uint64
		want  bool
	}{
		{"own field", model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}, fieldID, true},
		{"other tenant", model.Scope{TenantID: itest.TenantID + 1, FarmID: itest.FarmID}, fieldID, false},
		{"other farm", model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID + 1}, fieldID, false},
		{"unknown field", model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}, fieldID + 100000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fs.Owns(t.Context(), tt.scope, tt.field)
			if err != nil || got != tt.want {
				t.Fatalf("Owns = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestRedisIdemAndCache_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	idem := repository.NewRedisIdem(e.Redis)
	key := slot.Keys.IdemTool("ck")

	if ok, err := idem.SetNX(ctx, key, "PENDING:a", time.Minute); err != nil || !ok {
		t.Fatalf("SetNX first = %v, %v", ok, err)
	}
	if ok, err := idem.SetNX(ctx, key, "PENDING:b", time.Minute); err != nil || ok {
		t.Fatalf("SetNX second = %v, %v; want false", ok, err)
	}
	if ok, err := idem.Replace(ctx, key, "PENDING:b", "DONE:1", time.Minute); err != nil || ok {
		t.Fatalf("Replace wrong token = %v, %v; want false", ok, err)
	}
	if ok, err := idem.DeleteIf(ctx, key, "PENDING:b"); err != nil || ok {
		t.Fatalf("DeleteIf wrong token = %v, %v; want false", ok, err)
	}
	if ok, err := idem.Replace(ctx, key, "PENDING:a", "DONE:1", time.Minute); err != nil || !ok {
		t.Fatalf("Replace = %v, %v", ok, err)
	}
	if v, ok, err := idem.Get(ctx, key); err != nil || !ok || v != "DONE:1" {
		t.Fatalf("Get = %q, %v, %v", v, ok, err)
	}
	if ok, err := idem.DeleteIf(ctx, key, "DONE:1"); err != nil || !ok {
		t.Fatalf("DeleteIf = %v, %v", ok, err)
	}
	if _, ok, err := idem.Get(ctx, key); err != nil || ok {
		t.Fatalf("Get after delete = %v, %v; want miss", ok, err)
	}

	cache := repository.NewRedisCache(e.Redis)
	ck := slot.Keys.CacheMCP("weather.now", "d")
	if _, ok, err := cache.Get(ctx, ck); err != nil || ok {
		t.Fatalf("cache miss = %v, %v", ok, err)
	}
	if err := cache.Set(ctx, ck, []byte(`{"v":1}`), time.Minute); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := cache.Get(ctx, ck); err != nil || !ok || string(b) != `{"v":1}` {
		t.Fatalf("cache hit = %s, %v, %v", b, ok, err)
	}
}

// TestSummaryRepo_Integration：按 run 读回 tool_call.result_summary（跳过 NULL）与 run_event.payload，限定租户 / 农场。
func TestSummaryRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	runID := itest.InsertRun(t, slot.DB, t0)
	scope := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	calls := repository.NewToolCallRepo(slot.DB)
	base := model.ToolCallRow{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID, StepID: "enrich",
		ToolName: "field.profile", PolicyLevel: "read", ArgsDigest: "d", CreatedAt: t0}
	withSummary := base
	withSummary.CallKey, withSummary.Status, withSummary.ResultSummary = "s1", model.ToolStatusOK, json.RawMessage(`{"tel":"138****8000"}`)
	noSummary := base
	noSummary.CallKey, noSummary.Status, noSummary.ErrorCode = "s2", model.ToolStatusTimeout, "TIMEOUT"
	for _, r := range []model.ToolCallRow{withSummary, noSummary} {
		if _, err := calls.Save(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	ev := model.RunEvent{TenantID: itest.TenantID, FarmID: itest.FarmID, RunID: runID,
		Type: model.EventOrderTransition, Actor: model.ActorSystem, Payload: []byte(`{"lat":31.23}`), CreatedAt: t0}
	if err := slot.DB.WithContext(t.Context()).Transaction(func(tx *repository.DB) error {
		_, err := repository.EventRepo{}.AppendTx(t.Context(), tx, ev)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repository.SummaryRepo{}.ByRun(t.Context(), slot.DB, scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Source != repository.SourceToolCall || !strings.Contains(got[0].Text, "138****8000") ||
		got[1].Source != repository.SourceRunEvent || got[1].ID != 1 || !strings.Contains(got[1].Text, "31.23") {
		t.Fatalf("ByRun = %+v", got)
	}
	other, err := repository.SummaryRepo{}.ByRun(t.Context(), slot.DB, model.Scope{TenantID: itest.TenantID, FarmID: 2}, runID)
	if err != nil || len(other) != 0 {
		t.Fatalf("ByRun other farm = %+v, %v; want empty", other, err)
	}
}
