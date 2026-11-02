package sample

import (
	"fmt"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers"
	"agri-steward/internal/model"
)

// RuleFault 校验 P1 L6 故障样本：fixtures.mcp.*.cache、fixtures.model、fixtures.dispatch.advance 与 expect.degraded 的结构。
const RuleFault = "fault_shape"

// maxAdvance 是 fixtures.dispatch.advance 上限（越过 2h 下发时效即可，防止样本写错成大数）。
const maxAdvance = 24 * time.Hour

var (
	knownModelTasks     = set("classify", "choose", "explain", ModelTaskAll)
	knownFaults         = faultCodeSet()
	knownDegradeActions = set(string(model.DegradeRuleFallback), string(model.DegradeCacheFallback),
		string(model.DegradeRefuse), string(model.DegradeHoldNotify))
)

func faultCodeSet() map[string]bool {
	m := map[string]bool{}
	for _, c := range model.FaultCodes() {
		m[string(c)] = true
	}
	return m
}

// checkFault：cache 只用于有 max_staleness 的 server（mcp.DefaultCacheRule，与网关同源）且须带 RFC3339 collected_at；
// fixtures.model.unavailable 非空且取已知任务；dispatch.advance 是 (0, maxAdvance] 的时长；
// expect.degraded 的 fault / action 取矩阵枚举。
func checkFault(s *Sample) []string {
	var msgs []string
	for _, tool := range fixtureTools(s) {
		for i, f := range s.Fixtures.MCP[tool] {
			if f.Cache == nil {
				continue
			}
			if p := CacheProblem(tool, f.Cache); p != "" {
				msgs = append(msgs, fmt.Sprintf("fixtures.mcp.%s[%d].cache: %s", tool, i, p))
			}
		}
	}
	if m := s.Fixtures.Model; m != nil {
		if len(m.Unavailable) == 0 {
			msgs = append(msgs, "fixtures.model.unavailable must not be empty")
		}
		for i, task := range m.Unavailable {
			if !knownModelTasks[task] {
				msgs = append(msgs, fmt.Sprintf("fixtures.model.unavailable[%d] %q is not allowed", i, task))
			}
		}
	}
	if d := s.Fixtures.Dispatch; d != nil && d.Advance != "" {
		if _, err := d.AdvanceDuration(); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	for i, d := range s.Expect.Degraded {
		if !knownFaults[d.Fault] {
			msgs = append(msgs, fmt.Sprintf("expect.degraded[%d].fault %q is not allowed", i, d.Fault))
		}
		if !knownDegradeActions[d.Action] {
			msgs = append(msgs, fmt.Sprintf("expect.degraded[%d].action %q is not allowed", i, d.Action))
		}
	}
	return msgs
}

// AdvanceDuration 解析 fixtures.dispatch.advance：空串为 0；非法或不在 (0, maxAdvance] 返回错误。
func (d *DispatchFixture) AdvanceDuration() (time.Duration, error) {
	if d == nil || d.Advance == "" {
		return 0, nil
	}
	v, err := time.ParseDuration(d.Advance)
	if err != nil || v <= 0 || v > maxAdvance {
		return 0, fmt.Errorf("fixtures.dispatch.advance %q must be a duration in (0, %s]", d.Advance, maxAdvance)
	}
	return v, nil
}

// CacheProblem 返回缓存桩的结构问题（空串表示合法）：工具须是注册表里 Cacheable 且所在 server 有缓存窗的工具
// （与网关同一判定），值须是带 collected_at 的对象。
func CacheProblem(tool string, cache any) string {
	if _, ok := MaxStaleness(tool); !ok {
		return fmt.Sprintf("tool %q is not cacheable", tool)
	}
	if _, ok := CacheCollectedAt(cache); !ok {
		return "must be an object with RFC3339 collected_at"
	}
	return ""
}

// MaxStaleness 返回工具的超时回落上限：工具须在注册表（servers.Lookup）且 Cacheable，所在 server 的窗取
// mcp.DefaultCacheRule（与网关同一合并规则；configs/tools.yaml 与默认值一致由 cmd/eval 测试守住）。不可缓存返回 false。
func MaxStaleness(tool string) (time.Duration, bool) {
	server, spec, ok := servers.Lookup(tool)
	if !ok || !spec.Cacheable {
		return 0, false
	}
	fresh, limit := mcp.DefaultCacheRule().Window(server)
	return limit, fresh > 0
}

// CacheCollectedAt 取缓存桩的 collected_at（RFC3339）。
func CacheCollectedAt(cache any) (time.Time, bool) {
	m, ok := cache.(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	str, ok := m["collected_at"].(string)
	if !ok {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, str)
	return at, err == nil
}
