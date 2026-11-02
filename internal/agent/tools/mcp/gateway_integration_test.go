//go:build integration

package mcp_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers"
	"agri-steward/internal/config"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// TestGateway_CrossFarmField_Integration（P1 L1-B 验收 4）：真实 Gateway + mock Server + FieldScopeGuard + Redis 缓存，
// 农场 (1,1) 的调用带农场 (1,2) 的 field_id → REJECTED / SCOPE_DENIED，恰好一条 CROSS_FARM_DENIED 审计，且不写缓存；
// 再用本农场 field_id 调同一工具作对照：成功并写入 (1,1) 的缓存键。
func TestGateway_CrossFarmField_Integration(t *testing.T) {
	e := itest.Setup(t, "mcp")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	fieldOf := func(farm uint64) uint64 {
		var id uint64
		if err := slot.DB.WithContext(ctx).Raw("SELECT id FROM field_profile WHERE tenant_id=1 AND farm_id=? ORDER BY id LIMIT 1",
			farm).Scan(&id).Error; err != nil || id == 0 {
			t.Fatalf("farm %d field: id=%d err=%v", farm, id, err)
		}
		return id
	}
	own, foreign := fieldOf(1), fieldOf(2)

	cfg, err := config.LoadTools(filepath.Join(e.Root, "configs", "tools.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MockDir = filepath.Join(e.Root, cfg.MockDir)
	tp, err := policy.Load(filepath.Join(e.Root, "policy", "tool_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	srvs, err := servers.Build(cfg, slot.Clock, repository.FieldDirectory{DB: slot.DB})
	if err != nil {
		t.Fatal(err)
	}
	devices, err := servers.DeviceOwnership(cfg)
	if err != nil {
		t.Fatal(err)
	}
	levels := map[string]mcp.Level{}
	for _, s := range cfg.Servers() {
		levels[s.Name] = mcp.Level(s.Level)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := mcp.NewGateway(srvs, mcp.Options{Clock: slot.Clock, Logger: logger, Keys: slot.Keys, ToolPolicy: tp,
		Idem: repository.NewRedisIdem(e.Redis), Cache: repository.NewRedisCache(e.Redis),
		Calls: repository.NewToolCallRepo(slot.DB), Raw: &memRaw{}, Levels: levels,
		Fields:  repository.FieldScopeGuard{DB: slot.DB, Clock: slot.Clock, Logger: logger},
		Devices: devices})
	if err != nil {
		t.Fatal(err)
	}

	runID := itest.InsertRun(t, slot.DB, t0)
	farm1 := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}
	call := func(field uint64, seq int) agent.ToolResult {
		t.Helper()
		res, err := gw.Call(ctx, farm1, agent.ToolCall{RunID: runID, StepID: "enrich", Tool: "weather.forecast",
			Args: json.RawMessage(`{"field_id":` + strconv.FormatUint(field, 10) + `}`), CallSeq: seq})
		if err != nil {
			t.Fatalf("Call field %d: %v", field, err)
		}
		return res
	}
	cacheKeys := func() []string {
		t.Helper()
		keys, err := e.Redis.Keys(ctx, slot.Keys.Prefix()+"cache:mcp:*").Result()
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}

	res := call(foreign, 1)
	if res.Status != model.ToolStatusRejected || res.ErrorCode != string(mcp.ErrScopeDenied) {
		t.Fatalf("foreign field: status %s (%s), want REJECTED (SCOPE_DENIED)", res.Status, res.ErrorCode)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id=1 AND farm_id=1 AND run_id=? AND action=?
  AND result=? AND target=?`, runID, string(model.AuditCrossFarmDenied), model.AuditResultDenied,
		"field:"+strconv.FormatUint(foreign, 10)); n != 1 {
		t.Fatalf("CROSS_FARM_DENIED audit rows = %d, want 1", n)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM audit_log WHERE action=?", string(model.AuditCrossFarmDenied)); n != 1 {
		t.Fatalf("all CROSS_FARM_DENIED rows = %d, want 1", n)
	}
	if keys := cacheKeys(); len(keys) != 0 {
		t.Fatalf("cache written after scope denial: %v", keys)
	}

	if res := call(own, 2); res.Status != model.ToolStatusOK {
		t.Fatalf("own field: status %s (%s), want OK", res.Status, res.ErrorCode)
	}
	if keys := cacheKeys(); len(keys) != 1 {
		t.Fatalf("own field cache keys = %v, want 1 under farm (1,1)", keys)
	}
}
