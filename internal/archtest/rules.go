package archtest

import "strings"

// Rule 是一条分层规则：Applies 判断文件是否受约束，Bad 判断某条 import 是否违规。
type Rule struct {
	Name    string
	Applies func(f File) bool
	Bad     func(imp string) bool
	// NowOnly 非空时本规则改为检查 time.Now：只有该路径的文件可出现 time.Now。
	NowOnly string
}

func (r Rule) check(f File) []Violation {
	if r.NowOnly != "" {
		if f.Rel == r.NowOnly {
			return nil
		}
		out := make([]Violation, 0, len(f.NowAt))
		for _, line := range f.NowAt {
			out = append(out, Violation{File: f.Rel, Line: line, Rule: r.Name})
		}
		return out
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
}
