package main

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
	levels := map[string]mcp.Level{}
	timeouts := map[string]time.Duration{}
	for _, s := range cfg.Servers() {
		levels[s.Name] = mcp.Level(s.Level)
		timeouts[s.Name] = s.Timeout
	}
	gw, err := mcp.NewGateway(srv, mcp.Options{
		Clock: d.clk, Logger: d.logger, Keys: d.keys, ToolPolicy: tp,
		Idem: repository.NewRedisIdem(d.rdb), Cache: repository.NewRedisCache(d.rdb),
		Calls: repository.NewToolCallRepo(d.db), Raw: d.raw, Fields: repository.FieldScope{DB: d.db},
		Levels: levels, Timeouts: timeouts, Mask: pii.MaskJSON, RawReader: d.rawRead,
		// Events: L7 接 EventRepo。
	})
	if err != nil {
		return nil, fmt.Errorf("build mcp gateway: %w", err)
	}
	return gw, nil
}
