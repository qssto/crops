//go:build integration

package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

type refuseAgent struct{}

func (refuseAgent) Ask(context.Context, agent.Question) (agent.Answer, error) {
	return agent.Answer{Outcome: model.OutcomeRefused}, nil
}

type nopRaw struct{}

func (nopRaw) Put(context.Context, model.RawPut) (model.RawObject, error) {
	return model.RawObject{}, nil
}

type nopTools struct{}

func (nopTools) Call(context.Context, model.Scope, agent.ToolCall) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

type itPresign struct{}

func (itPresign) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://minio.it/" + key + "?sig=1", nil
}

// farmB 是租户 1 / 农场 2 上的一组资源（run / raw_object / 作业单 / OPEN 票 / 田块）。
type farmB struct{ run, raw, ticket, field uint64 }

func seedFarmB(t *testing.T, slot *env.Slot) farmB {
	t.Helper()
	db := slot.DB.WithContext(t.Context())
	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	var b farmB
	b.field = uint64(itest.Count(t, slot.DB, `SELECT id FROM field_profile WHERE tenant_id=1 AND farm_id=2
  AND ext_field_code='field-201'`))
	exec(`INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version, model_tier,
  knowledge_ref, interrupt_type, created_at, updated_at)
  VALUES (1, 2, ?, 2001, 'INTERRUPTED', 'g1', 'p1', 'small', 'kb@test', 'WAIT_APPROVAL', ?, ?)`, b.field, t0, t0)
	b.run = uint64(itest.Count(t, slot.DB, `SELECT MAX(id) FROM agent_run WHERE farm_id=2`))
	exec(`INSERT INTO raw_object (tenant_id, farm_id, run_id, kind, object_key, sha256, size_bytes, created_at)
  VALUES (1, 2, ?, 'LLM_IO', 'raw/it/farm2', REPEAT('b', 64), 10, ?)`, b.run, t0)
	b.raw = uint64(itest.Count(t, slot.DB, `SELECT MAX(id) FROM raw_object WHERE farm_id=2`))
	exec(`INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload,
  time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (1, 2, ?, ?, ?, 'FERTILIZE', 'DRONE', '{}', ?, ?, 'PENDING_APPROVAL', 'DRY_RUN', 1, ?, ?)`,
		b.field, b.run, b.run, t0, t0.Add(time.Hour), t0, t0)
	order := itest.Count(t, slot.DB, `SELECT MAX(id) FROM work_order WHERE farm_id=2`)
	exec(`INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_seq, risk_level, required_role,
  confirm_required, initiator_id, snapshot_digest, snapshot_raw_id, status, expires_at, created_at, updated_at)
  VALUES (1, 2, ?, ?, 1, 'LOW', '["farm_manager","agronomist"]', 0, 2001, REPEAT('7', 64), ?, 'OPEN', ?, ?, ?)`,
		b.run, order, b.raw, t0.Add(time.Hour), t0, t0)
	b.ticket = uint64(itest.Count(t, slot.DB, `SELECT MAX(id) FROM approval_ticket WHERE farm_id=2`))
	return b
}

// 验收 1 / 3：农场 A 身份访问农场 B 的 run / raw / events / approvals / decide / field 100% 拒绝，
// 每次拒绝落一行 CROSS_FARM_DENIED（路径越界记在 (0,0)，资源越界记在本农场）。
func TestCrossFarmDenied_Integration(t *testing.T) {
	e := itest.Setup(t, "app")
	slot := e.Fresh(t, t0)
	b := seedFarmB(t, slot)
	perms, err := service.LoadPermissionMatrix(filepath.Join(e.Root, "policy/permission_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	risk, err := policy.LoadRiskMatrix(filepath.Join(e.Root, "policy/risk_matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Build(app.Deps{Clock: slot.Clock, Logger: logger, DB: slot.DB, Redis: e.Redis, Keys: slot.Keys,
		RawStore: nopRaw{}, Agent: refuseAgent{}, Tools: nopTools{}, Risk: risk, Presign: itPresign{}, Permissions: perms})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := a.NewAuth(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	h := a.Handler(auth)
	bearer := func(uid, tenant uint64) string {
		tok, err := auth.Issue(uid, tenant, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + tok
	}
	id := func(v uint64) string { return strconv.FormatUint(v, 10) }
	decideBody := `{"decision":"APPROVE","snapshot_digest":"` + strings.Repeat("7", 64) + `"}`
	askBody := fmt.Sprintf(`{"field_id":%d,"text":"要不要追肥"}`, b.field)
	farm2, farm1 := "/api/v1/farms/2", "/api/v1/farms/1"

	tests := []struct {
		name, method, path, body string
		wantCode                 string // CROSS_FARM_DENIED summary.code
		wantTarget               string
		wantFarm                 uint64 // 审计行 farm_id
	}{
		{"path run", http.MethodGet, farm2 + "/runs/" + id(b.run), "", service.DenyScope, "farm:2", 0},
		{"path raw", http.MethodGet, farm2 + "/runs/" + id(b.run) + "/raw/" + id(b.raw), "", service.DenyScope, "farm:2", 0},
		{"path events", http.MethodGet, farm2 + "/runs/" + id(b.run) + "/events", "", service.DenyScope, "farm:2", 0},
		{"path approvals", http.MethodGet, farm2 + "/approvals", "", service.DenyScope, "farm:2", 0},
		{"path decide", http.MethodPost, farm2 + "/approvals/" + id(b.ticket) + "/decide", decideBody, service.DenyScope, "farm:2", 0},
		{"path ask", http.MethodPost, farm2 + "/ask", askBody, service.DenyScope, "farm:2", 0},
		{"idor run", http.MethodGet, farm1 + "/runs/" + id(b.run), "", service.DenyResource, "run:" + id(b.run), 1},
		{"idor raw", http.MethodGet, farm1 + "/runs/" + id(b.run) + "/raw/" + id(b.raw), "", service.DenyResource,
			"raw:" + id(b.raw), 1},
		{"idor events", http.MethodGet, farm1 + "/runs/" + id(b.run) + "/events", "", service.DenyResource, "run:" + id(b.run), 1},
		{"idor decide", http.MethodPost, farm1 + "/approvals/" + id(b.ticket) + "/decide", decideBody, service.DenyResource,
			"ticket:" + id(b.ticket), 1},
		{"idor field", http.MethodPost, farm1 + "/ask", askBody, service.DenyResource, "field:" + id(b.field), 1},
	}
	// 同一 target 会被多个用例命中（farm:2、run:N），按每个请求前后的增量核对恰好一行。
	denied := func(t *testing.T, farm uint64, target, code string) int64 {
		return itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action='CROSS_FARM_DENIED' AND result='DENIED'
  AND actor_type='user' AND actor_id=1002 AND tenant_id=? AND farm_id=? AND target=?
  AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.code'))=?`, farm, farm, target, code)
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := denied(t, tt.wantFarm, tt.wantTarget, tt.wantCode)
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", bearer(1002, 1)) // 农场 1 农艺师：角色足够，只因越界被拒
			req.Header.Set("X-Request-Id", "it-cross-"+strconv.Itoa(i))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
			}
			if n := denied(t, tt.wantFarm, tt.wantTarget, tt.wantCode) - before; n != 1 {
				t.Fatalf("new CROSS_FARM_DENIED rows for %s = %d, want 1", tt.wantTarget, n)
			}
		})
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action='CROSS_FARM_DENIED'`); n != int64(len(tests)) {
		t.Fatalf("CROSS_FARM_DENIED rows = %d, want %d", n, len(tests))
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM approval_decision`); n != 0 {
		t.Fatalf("denied decide must not write approval_decision, rows = %d", n)
	}

	// 对照：农场 B 自己的成员与平台管理员可读；农场 1 的待审批列表不含农场 B 的票。
	for _, c := range []struct {
		name, auth, path string
	}{
		{"farm B manager", bearer(2001, 1), farm2 + "/runs/" + id(b.run)},
		{"platform admin", bearer(9001, 0), farm2 + "/runs/" + id(b.run)},
		{"farm A pending list", bearer(1002, 1), farm1 + "/approvals"},
	} {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("Authorization", c.auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), fmt.Sprintf(`"ticket_id":%d`, b.ticket)) &&
			c.name == "farm A pending list" {
			t.Fatalf("%s: status = %d body=%s", c.name, rec.Code, rec.Body.String())
		}
	}
}
