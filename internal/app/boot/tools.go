package boot

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers"
	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
	"agri-steward/internal/service"
)

// toolDeps 是装配 gateway 所需的基础设施。
type toolDeps struct {
	clk    clock.Clock
	logger *slog.Logger
	db     *repository.DB
	rdb    redis.UniversalClient
	keys   rediskey.Keys
	raw    agent.RawStore
	// rawRead 读回 raw 对象：去重命中时向模型返回原始结果（result_summary 已脱敏）。
	rawRead mcp.RawReader
}

// buildTools 读 tools 配置并按 mode 装配四个 MCP Server，再用 mcp gateway 包装
// （注册表、策略、租户校验、去重缓存、超时重试、错误码归一、tool_call 落库）。
// policyPath 是 tool_policy.yaml：与注册表比对不符时返回错误，进程退出（P0 §6.6 验收 1）。
// P1-L4-A 自 cmd/server/tools.go 原样迁入，server 与 worker 共用。
func buildTools(path, policyPath string, d toolDeps) (agent.ToolProvider, error) {
	cfg, err := config.LoadTools(path)
	if err != nil {
		return nil, err
	}
	tp, err := policy.Load(policyPath)
	if err != nil {
		return nil, err
	}
	srv, err := servers.Build(cfg, d.clk, repository.FieldDirectory{DB: d.db})
	if err != nil {
		return nil, fmt.Errorf("build mcp servers: %w", err)
	}
	devices, err := servers.DeviceOwnership(cfg)
	if err != nil {
		return nil, err
	}
	levels := map[string]mcp.Level{}
	timeouts := map[string]time.Duration{}
	for _, s := range cfg.Servers() {
		levels[s.Name] = mcp.Level(s.Level)
		timeouts[s.Name] = s.Timeout
	}
	fresh, maxStale := cfg.CacheWindows() // P1 L6：缓存新鲜期与超时回落上限
	gw, err := mcp.NewGateway(srv, mcp.Options{
		Clock: d.clk, Logger: d.logger, Keys: d.keys, ToolPolicy: tp,
		Idem: repository.NewRedisIdem(d.rdb), Cache: repository.NewRedisCache(d.rdb),
		Calls: repository.NewToolCallRepo(d.db), Raw: d.raw,
		Fields:  repository.FieldScopeGuard{DB: d.db, Clock: d.clk, Logger: d.logger}, // 越界写 CROSS_FARM_DENIED 审计
		Devices: devices,                                                              // device_id 按显式绑定判归属
		Levels:  levels, Timeouts: timeouts, Mask: pii.MaskJSON, RawReader: d.rawRead,
		CacheTTL: fresh, MaxStaleness: maxStale,
		// Events: L7 接 EventRepo。
	})
	if err != nil {
		return nil, fmt.Errorf("build mcp gateway: %w", err)
	}
	return gw, nil
}

// fieldHistoryWriter 按 tools.field.mode 选 field.history 回写写端（P1-L4-B）：mock → 内存幂等实现；
// real → mcp-field 写接口待对接，失败关闭为告警（outbox 退避重试）。写端只给 outbox 处理器用，不对模型可见。
func fieldHistoryWriter(path string) (service.FieldHistoryWriter, error) {
	cfg, err := config.LoadTools(path)
	if err != nil {
		return nil, err
	}
	if cfg.Field.Mode == config.ToolModeReal {
		return service.UnwiredFieldHistory{}, nil
	}
	return service.NewMemFieldHistory(), nil
}
