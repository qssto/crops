package mcp

import (
	"fmt"
	"maps"
	"time"

	"agri-steward/internal/agent"
)

// CacheRule 是 cache:mcp 的时间窗与超时回落判定（P1 L6），网关与评测夹具（evalkit/env.FixtureMCP）共用同一份，
// 保证评测回落行为与生产一致。构造后只读。
type CacheRule struct {
	fresh    map[string]time.Duration // server 名 → 正常读新鲜期；0 / 缺失表示不缓存
	maxStale map[string]time.Duration // server 名 → 回落最大陈旧度（亦为缓存写入 TTL），≥ 新鲜期
}

// NewCacheRule 把按 server 的覆盖值逐项合并到 DefaultCacheTTL / DefaultMaxStaleness 上（只配了部分 server 时，
// 其余 server 仍取默认，不会悄悄失去缓存）；最大陈旧度缺省取新鲜期，小于新鲜期返回错误。
func NewCacheRule(fresh, maxStale map[string]time.Duration) (CacheRule, error) {
	r := CacheRule{fresh: maps.Clone(DefaultCacheTTL), maxStale: maps.Clone(DefaultMaxStaleness)}
	maps.Copy(r.fresh, fresh)
	maps.Copy(r.maxStale, maxStale)
	for server, ttl := range r.fresh {
		ms, ok := r.maxStale[server]
		if !ok {
			r.maxStale[server] = ttl
			continue
		}
		if ms < ttl {
			return CacheRule{}, fmt.Errorf("mcp cache: server %s max_staleness %s < cache ttl %s", server, ms, ttl)
		}
	}
	return r, nil
}

// DefaultCacheRule 是只用默认时间窗的判定（默认值自洽，不会出错）。
func DefaultCacheRule() CacheRule {
	r, err := NewCacheRule(nil, nil)
	if err != nil {
		panic(err) // 默认表写错属于编码错误
	}
	return r
}

// Window 返回 server 的新鲜期与最大陈旧度；新鲜期为 0 表示该 server 不缓存（也不回落）。
func (r CacheRule) Window(server string) (fresh, maxStale time.Duration) {
	fresh = r.fresh[server]
	if fresh <= 0 {
		return 0, 0
	}
	return fresh, r.maxStale[server]
}

// Bypass 表示本次调用不读缓存、也不回落（P0 §6.3 流程 3、P1 §6.7）：下发前复检（ToolCall.Recheck）、
// pre_dispatch step，以及 epoch>0 的时效工具调用。
func Bypass(call agent.ToolCall, timeSensitive bool) bool {
	return call.Recheck || call.StepID == PreDispatchStep || (timeSensitive && call.Epoch > 0)
}

// FallbackAge 判定一次 TIMEOUT / UPSTREAM 能否回落到 collectedAt 采集的缓存：工具可缓存、server 配了时间窗、
// 调用未 Bypass，且 now - collectedAt ≤ 最大陈旧度。返回陈旧度（不早于 0）与是否可回落。
func (r CacheRule) FallbackAge(server string, spec ToolSpec, call agent.ToolCall, now, collectedAt time.Time) (time.Duration, bool) {
	fresh, limit := r.Window(server)
	if !spec.Cacheable || fresh <= 0 || Bypass(call, spec.TimeSensitive) {
		return 0, false
	}
	age := max(now.Sub(collectedAt), 0)
	return age, age <= limit
}
