package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ToolPolicyVersion 是本包支持的 tool_policy.yaml 版本。
const ToolPolicyVersion = 1

// 启动期自检失败的原因（Check / Bind 返回的错误包装它们，调用方据此退出进程）。
var (
	ErrBadToolPolicy  = errors.New("policy: invalid tool_policy")
	ErrLevelMismatch  = errors.New("policy: registry level differs from tool_policy")
	ErrActuateVisible = errors.New("policy: actuate tool is visible to the model")
	ErrPolicyStale    = errors.New("policy: declared tool not in registry")
	// ErrActuateDenied 是来自模型的 actuate 调用（兜底拒绝并告警）；同时满足 errors.Is(err, ErrLevelDenied)。
	ErrActuateDenied = errors.New("policy: actuate call from agent rejected")
)

// ToolPolicy 是 policy/tool_policy.yaml：工具名 → 等级。构造后只读，并发安全。
type ToolPolicy struct {
	levels map[string]Level
}

// toolPolicyFile 是 tool_policy.yaml 的结构。
type toolPolicyFile struct {
	Version int `yaml:"version"`
	Tools   []struct {
		Name  string `yaml:"name"`
		Level Level  `yaml:"level"`
	} `yaml:"tools"`
}

// Load 读取并校验 tool_policy.yaml。
func Load(path string) (*ToolPolicy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read tool_policy: %w", err)
	}
	tp, err := ParseToolPolicy(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return tp, nil
}

// ParseToolPolicy 解析 tool_policy 内容：版本不符、未知字段、工具名为空或重名、等级不是 read / write / actuate 都报错。
func ParseToolPolicy(b []byte) (*ToolPolicy, error) {
	var f toolPolicyFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadToolPolicy, err)
	}
	if f.Version != ToolPolicyVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadToolPolicy, f.Version, ToolPolicyVersion)
	}
	tp := &ToolPolicy{levels: make(map[string]Level, len(f.Tools))}
	var errs []error
	for i, t := range f.Tools {
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("%w: tools[%d] has no name", ErrBadToolPolicy, i))
		case !t.Level.valid():
			errs = append(errs, fmt.Errorf("%w: %s level %q, want read / write / actuate", ErrBadToolPolicy, t.Name, t.Level))
		default:
			if _, dup := tp.levels[t.Name]; dup {
				errs = append(errs, fmt.Errorf("%w: duplicate tool %q", ErrBadToolPolicy, t.Name))
				continue
			}
			tp.levels[t.Name] = t.Level
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if len(tp.levels) == 0 {
		return nil, fmt.Errorf("%w: no tools declared", ErrBadToolPolicy)
	}
	return tp, nil
}

func (l Level) valid() bool { return l == LevelRead || l == LevelWrite || l == LevelActuate }

// Level 返回工具在策略文件中声明的等级。
func (tp *ToolPolicy) Level(tool string) (Level, bool) {
	l, ok := tp.levels[tool]
	return l, ok
}

// Check 是启动期自检：registry 是模型可见工具集（mcp 工具注册表，Level 为注册表等级）。逐项比对：
// 注册表工具未在策略中声明等级、或注册表自身等级为空 → ErrLevelUndeclared；两边等级不一致 → ErrLevelMismatch；
// 注册表中出现 actuate 工具 → ErrActuateVisible；策略中的 read / write 工具不在注册表 → ErrPolicyStale。
// 返回全部问题（errors.Join），任一即启动失败。
func (tp *ToolPolicy) Check(registry []Spec) error {
	var errs []error
	seen := make(map[string]bool, len(registry))
	for _, s := range registry {
		seen[s.Tool] = true
		declared, ok := tp.levels[s.Tool]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%w: %s is registered but not declared in tool_policy", ErrLevelUndeclared, s.Tool))
		case s.Level == "":
			errs = append(errs, fmt.Errorf("%w: %s has no level in the registry (tool_policy says %s)", ErrLevelUndeclared, s.Tool, declared))
		case declared != s.Level:
			errs = append(errs, fmt.Errorf("%w: %s is %s in registry, %s in tool_policy", ErrLevelMismatch, s.Tool, s.Level, declared))
		}
		if declared == LevelActuate || s.Level == LevelActuate {
			errs = append(errs, fmt.Errorf("%w: %s", ErrActuateVisible, s.Tool))
		}
	}
	for _, name := range tp.sortedNames() {
		if l := tp.levels[name]; l != LevelActuate && !seen[name] {
			errs = append(errs, fmt.Errorf("%w: %s (%s)", ErrPolicyStale, name, l))
		}
	}
	return errors.Join(errs...)
}

// Actuate 返回策略中声明为 actuate 的工具名（排序）。
func (tp *ToolPolicy) Actuate() []string {
	var out []string
	for _, name := range tp.sortedNames() {
		if tp.levels[name] == LevelActuate {
			out = append(out, name)
		}
	}
	return out
}

func (tp *ToolPolicy) sortedNames() []string {
	names := make([]string, 0, len(tp.levels))
	for n := range tp.levels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Bind 先做启动期自检（Check），再构造调用期策略表：除 New 的校验外，策略中声明为 actuate 的工具
// 即使未注册，来自模型的调用也返回 ErrActuateDenied（而非 ErrUnknownTool），供调用方告警。
func (tp *ToolPolicy) Bind(specs []Spec, allowed ...Level) (*Policy, error) {
	if err := tp.Check(specs); err != nil {
		return nil, fmt.Errorf("policy: startup self-check failed:\n%w", err)
	}
	for _, l := range allowed {
		if l == LevelActuate {
			return nil, fmt.Errorf("%w: actuate must never be allowed for agent calls", ErrBadToolPolicy)
		}
	}
	p, err := New(specs, allowed...)
	if err != nil {
		return nil, err
	}
	p.actuate = make(map[string]bool)
	for _, name := range tp.Actuate() {
		p.actuate[name] = true
	}
	return p, nil
}

// actuateDenied 构造 actuate 拒绝错误（同时匹配 ErrActuateDenied 与 ErrLevelDenied）。
func actuateDenied(tool string) error {
	return fmt.Errorf("%w (%w): %s", ErrActuateDenied, ErrLevelDenied, strings.TrimSpace(tool))
}
