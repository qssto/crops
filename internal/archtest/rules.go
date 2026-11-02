package archtest

import (
	"slices"
	"strings"
)

// Rule 是一条分层规则：Applies 判断文件是否受约束，Bad 判断某条 import 是否违规。
type Rule struct {
	Name    string
	Applies func(f File) bool
	Bad     func(imp string) bool
	// NowOnly 非空时本规则改为检查 time.Now：只有该路径的文件可出现 time.Now。
	NowOnly string
	// SysOnly 非空时本规则改为检查 repository.NewSystemScope：只有白名单文件可构造。
	SysOnly []string
}

func (r Rule) check(f File) []Violation {
	if r.SysOnly != nil {
		return r.lines(f, f.SysAt, slices.Contains(r.SysOnly, f.Rel))
	}
	if r.NowOnly != "" {
		return r.lines(f, f.NowAt, f.Rel == r.NowOnly)
	}
	if !r.Applies(f) {
		return nil
	}
	var out []Violation
	for _, imp := range f.Imports {
		if r.Bad(imp) {
			out = append(out, Violation{File: f.Rel, Rule: r.Name + ": imports " + imp})
		}
	}
	return out
}

// lines 把选择子出现的行号转成违规；allowed 为真（白名单文件）时放行。
func (r Rule) lines(f File, at []int, allowed bool) []Violation {
	if allowed {
		return nil
	}
	out := make([]Violation, 0, len(at))
	for _, line := range at {
		out = append(out, Violation{File: f.Rel, Line: line, Rule: r.Name})
	}
	return out
}

// under 判断 pkg 是否等于 dir 或在其子目录下。
func under(pkg, dir string) bool { return pkg == dir || strings.HasPrefix(pkg, dir+"/") }

func in(dirs ...string) func(File) bool {
	return func(f File) bool {
		for _, d := range dirs {
			if under(f.Pkg, d) {
				return true
			}
		}
		return false
	}
}

func notIn(dirs ...string) func(File) bool {
	inside := in(dirs...)
	return func(f File) bool { return !inside(f) }
}

// internalPkg 返回模块内包的相对目录（internal/xxx），非模块内包返回 ""。
func internalPkg(imp string) string {
	if rest, ok := strings.CutPrefix(imp, Module+"/"); ok {
		return rest
	}
	return ""
}

func importsAny(dirs ...string) func(string) bool {
	return func(imp string) bool {
		p := internalPkg(imp)
		if p == "" {
			return false
		}
		for _, d := range dirs {
			if under(p, d) {
				return true
			}
		}
		return false
	}
}

func isStdlib(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".") && first != Module
}

func hasPrefix(prefixes ...string) func(string) bool {
	return func(imp string) bool {
		for _, p := range prefixes {
			if imp == p || strings.HasPrefix(imp, p+"/") {
				return true
			}
		}
		return false
	}
}

// Rules 是 L1 冻结的分层规则；新增规则须同步 docs/contracts.md。
var Rules = []Rule{
	{Name: "agent must not import controller/service",
		Applies: in("internal/agent"), Bad: importsAny("internal/controller", "internal/service")},
	{Name: "agent/harness must not import graph/knowledge/calc/guardrail/service/controller/repository",
		Applies: in("internal/agent/harness"),
		Bad: importsAny("internal/agent/graph", "internal/agent/knowledge", "internal/agent/calc",
			"internal/agent/guardrail", "internal/service", "internal/controller", "internal/repository")},
	{Name: "production code must not import evalkit",
		Applies: notIn("internal/evalkit", "cmd/eval"), Bad: importsAny("internal/evalkit")},
	{Name: "model imports stdlib only",
		Applies: in("internal/model"), Bad: func(imp string) bool { return !isStdlib(imp) }},
	{Name: "controller may import only service and model (internal)",
		Applies: in("internal/controller"),
		Bad: func(imp string) bool {
			p := internalPkg(imp)
			return p != "" && !under(p, "internal/service") && !under(p, "internal/model")
		}},
	{Name: "only repository/app/cmd may import gorm",
		Applies: notIn("internal/repository", "internal/app", "cmd"), Bad: hasPrefix("gorm.io")},
	{Name: "only config may import viper",
		Applies: notIn("internal/config"), Bad: hasPrefix("github.com/spf13/viper")},
	{Name: "only controller may import gin",
		Applies: notIn("internal/controller"), Bad: hasPrefix("github.com/gin-gonic/gin")},
	{Name: "time.Now only in internal/clock/system.go (use clock.Clock)", NowOnly: "internal/clock/system.go"},
	// L8：设备 Adapter 只由下发器调用（母文档 §4.6.2-2）；app / cmd 装配、evalkit 注入 RecordingAdapter 除外。
	{Name: "only service/workorder_dispatch.go may import adapter",
		Applies: func(f File) bool {
			return f.Rel != adapterCaller && notIn("internal/adapter", "internal/app", "internal/evalkit", "cmd")(f)
		},
		Bad: importsAny("internal/adapter")},
	// P1-L1-B：跨农场扫描凭证只许白名单文件构造（P1 手册 §6.2 流程 3）。
	// 收紧计划：P1-L4 扫描与 outbox 轮询迁到 cmd/worker 后，白名单改为只含 cmd/worker。
	{Name: "only scan/outbox pollers may construct repository.SystemScope", SysOnly: systemScopeCallers},
	// P1-L2：Adapter 只翻译协议、不碰库也不碰 Agent（P1 手册 §6.3）；原始指令 / 回执经 Traced 注入的
	// RawPutter / CallRecorder 接口落库，台账由下发器写，避免设备层反向依赖持久化与编排。
	{Name: "adapter must not import repository/agent",
		Applies: in("internal/adapter"), Bad: importsAny("internal/repository", "internal/agent")},
	// P1-L3：约束校验器是纯函数（入参即全部观测），取数在 graph / service，便于单测与 L4 下发前复用（P1 手册 §6.4）。
	{Name: "guardrail/constraint must not import repository/service/tools/adapter or do I/O",
		Applies: in("internal/agent/guardrail/constraint"),
		Bad: func(imp string) bool {
			return importsAny("internal/repository", "internal/service", "internal/agent/tools", "internal/adapter")(imp) ||
				hasPrefix("os", "net", "database/sql", "io/fs")(imp)
		}},
}

// systemScopeCallers 是可构造 repository.SystemScope 的文件白名单（server 内扫描与 outbox 轮询）。
var systemScopeCallers = []string{"internal/service/scan.go", "internal/service/outbox.go",
	"internal/service/receipt.go"} // P1-L4-B：回执 webhook 反查与回执超时扫描

// adapterCaller 是唯一可调用设备 Adapter 的业务文件（WorkOrderService.Dispatch）。
const adapterCaller = "internal/service/workorder_dispatch.go"
