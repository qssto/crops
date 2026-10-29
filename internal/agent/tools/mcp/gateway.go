package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/clock"
)

// 默认参数（P0 §6.3 流程 3、③ Redis 键）。
const (
	DefaultTimeout = 3 * time.Second
	// readRetries 是 read 且幂等工具超时 / 上游失败后的重试次数。
	readRetries        = 2
	defaultPendingPoll = 100 * time.Millisecond
	doneTTL            = 24 * time.Hour
	// PreDispatchStep 是下发前复检的 step_id：一律不读缓存（结果仍回写）。
	PreDispatchStep = "pre_dispatch"
)

// DefaultCacheTTL 是 cache:mcp 的默认 TTL（server 名 → TTL）；field / device 不缓存。
var DefaultCacheTTL = map[string]time.Duration{"weather": 10 * time.Minute, "crop": 24 * time.Hour}

// Options 是 gateway 的依赖与参数。Events、Mask、Levels、Timeouts、CacheTTL、PendingPoll 可省略；
// 给了 Mask 就必须给 RawReader。
type Options struct {
	Clock  clock.Clock
	Logger *slog.Logger
	// ToolPolicy 是 policy/tool_policy.yaml；构造时与注册表逐项比对，不符即构造失败（启动失败）。
	ToolPolicy *policy.ToolPolicy
	Keys       Keys
	Idem       IdemStore
	Cache      Cache
	Calls      agent.ToolCallStore
	Raw        agent.RawStore
	Fields     FieldScope
	// Events 非 nil 时每次调用发 TOOL_CALL_END（L7 接 EventRepo）。
	Events agent.EventSink
	// Mask 对 result_summary 脱敏；nil 时原样（生产注入 pii.MaskJSON）。
	Mask func(json.RawMessage) json.RawMessage
	// RawReader 读回 RawStore 写入的原始结果，去重命中时向模型返回原值而非脱敏摘要；Mask 非 nil 时必填。
	RawReader RawReader
	// Levels 是 server 名 → 策略等级，覆盖 ToolSpec.Level（来自 configs/tools.yaml 的 level）。
	Levels map[string]Level
	// Timeouts 是 server 名 → 单次调用超时，缺省 DefaultTimeout。
	Timeouts map[string]time.Duration
	// CacheTTL 是 server 名 → 缓存 TTL，nil 时用 DefaultCacheTTL；只对 Cacheable 工具生效。
	CacheTTL map[string]time.Duration
	// PendingPoll 是等待在途同 key 调用时的轮询间隔。
	PendingPoll time.Duration
}

func (o Options) validate() error {
	var errs []error
	req := func(missing bool, name string) {
		if missing {
			errs = append(errs, fmt.Errorf("mcp gateway: %s is required", name))
		}
	}
	req(o.Clock == nil, "Clock")
	req(o.Logger == nil, "Logger")
	req(o.ToolPolicy == nil, "ToolPolicy")
	req(o.Keys == nil, "Keys")
	req(o.Idem == nil, "Idem")
	req(o.Cache == nil, "Cache")
	req(o.Calls == nil, "Calls")
	req(o.Raw == nil, "Raw")
	req(o.Fields == nil, "Fields")
	if o.Mask != nil && o.RawReader == nil {
		errs = append(errs, errors.New("mcp gateway: RawReader is required when Mask is set (dedup replay must return unmasked results)"))
	}
	return errors.Join(errs...)
}

// tool 是注册表中的一个工具。
type tool struct {
	server   Server
	spec     ToolSpec
	level    Level
	output   policy.Validator // 出参 data 的 schema；nil 时只要求 JSON 对象
	timeout  time.Duration
	cacheTTL time.Duration // 0 表示不缓存
}

// tries 是总调用次数：read 且幂等的工具多重试 readRetries 次。
func (t *tool) tries() int {
	if t.spec.Idempotent && t.level == LevelRead {
		return 1 + readRetries
	}
	return 1
}

// pendingTTL 是 idem:tool 占位 TTL：单次超时 × 调用次数（3s × 3 = 9s）。
func (t *tool) pendingTTL() time.Duration { return t.timeout * time.Duration(t.tries()) }

// Gateway 是生产 agent.ToolProvider：注册表 → policy.Before → 租户校验 → 去重 → 缓存 → 调用 → policy.After → 落行。
// 构造后只读，并发安全。
type Gateway struct {
	opt    Options
	tools  map[string]*tool
	policy *policy.Policy
	mask   func(json.RawMessage) json.RawMessage
}

// allowedLevels 是 P0 Agent 可直接调用的等级：write / actuate 只经 service/workorder（母文档 §4.6.2-2）。
var allowedLevels = []policy.Level{policy.LevelRead}

// NewGateway 注册全部 Server 的工具：工具重名、schema 编译失败都报错；再与 ToolPolicy 做启动期自检
// （注册表工具未声明等级或等级不符、模型可见工具集含 actuate、策略 read / write 条目不在注册表，任一即报错）。
func NewGateway(servers []Server, opt Options) (*Gateway, error) {
	if err := opt.validate(); err != nil {
		return nil, err
	}
	if opt.CacheTTL == nil {
		opt.CacheTTL = DefaultCacheTTL
	}
	if opt.PendingPoll <= 0 {
		opt.PendingPoll = defaultPendingPoll
	}
	g := &Gateway{opt: opt, tools: map[string]*tool{}, mask: opt.Mask}
	if g.mask == nil {
		g.mask = func(m json.RawMessage) json.RawMessage { return m }
	}
	var specs []policy.Spec
	var errs []error
	for _, srv := range servers {
		for _, sp := range srv.Tools() {
			t, ps, err := g.register(srv, sp)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			g.tools[sp.Name] = t
			specs = append(specs, ps)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	p, err := opt.ToolPolicy.Bind(specs, allowedLevels...)
	if err != nil {
		return nil, fmt.Errorf("mcp gateway: %w", err)
	}
	g.policy = p
	return g, nil
}

func (g *Gateway) register(srv Server, sp ToolSpec) (*tool, policy.Spec, error) {
	if _, dup := g.tools[sp.Name]; dup {
		return nil, policy.Spec{}, fmt.Errorf("mcp gateway: duplicate tool %q (server %s)", sp.Name, srv.Name())
	}
	in, err := policy.CompileSchema(sp.Name+"#input", sp.InputSchema)
	if err != nil {
		return nil, policy.Spec{}, fmt.Errorf("mcp gateway: %w", err)
	}
	t := &tool{server: srv, spec: sp, level: sp.Level, timeout: DefaultTimeout}
	if l, ok := g.opt.Levels[srv.Name()]; ok {
		t.level = l
	}
	if d, ok := g.opt.Timeouts[srv.Name()]; ok && d > 0 {
		t.timeout = d
	}
	if sp.Cacheable {
		t.cacheTTL = g.opt.CacheTTL[srv.Name()]
	}
	if len(sp.OutputSchema) > 0 {
		if t.output, err = policy.CompileSchema(sp.Name+"#output", sp.OutputSchema); err != nil {
			return nil, policy.Spec{}, fmt.Errorf("mcp gateway: %w", err)
		}
	}
	return t, policy.Spec{Tool: sp.Name, Level: policy.Level(t.level), Args: in}, nil
}

// Tools 返回已注册工具的声明（供 Agent 发现，副本）。
func (g *Gateway) Tools() []ToolSpec {
	out := make([]ToolSpec, 0, len(g.tools))
	for _, t := range g.tools {
		sp := t.spec
		sp.Level = t.level
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
