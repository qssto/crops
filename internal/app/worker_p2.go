package app

import (
	"fmt"
)

// ---- P2 第 3 批接缝：worker 后台任务登记 ----
// L3（quality-metric-rollup / online-judge-sample）、L4（rule_shadow_diff 保留期清理）、L5（kb_load_item 保留期清理）
// 各在自己的新文件里用 init 调 registerP2Loops 登记，不改 worker.go 的 WorkerLoops。未登记时不加任何任务（行为不变）。

// p2LoopBuilder 按已装配的 App 构造一组后台任务；依赖未注入时返回 nil, nil（不起任务），配置错误返回 error（worker 启动失败）。
type p2LoopBuilder func(a *App) ([]Loop, error)

type p2LoopEntry struct {
	name  string
	build p2LoopBuilder
}

// p2Loops 只在包初始化（init）时追加，之后只读；同包 init 按文件名顺序执行，顺序确定。
var p2Loops []p2LoopEntry

// registerP2Loops 登记一组 P2 后台任务。name 用于报错定位（建议 "P2-L3" 这类层号）；重名与空构造在 p2WorkerLoops 报错。
func registerP2Loops(name string, build p2LoopBuilder) {
	p2Loops = append(p2Loops, p2LoopEntry{name: name, build: build})
}

// p2WorkerLoops 依次构造已登记的 P2 任务。重名登记、空构造、空 Run、任务名重复返回错误。
func (a *App) p2WorkerLoops() ([]Loop, error) {
	return buildP2Loops(a, p2Loops)
}

func buildP2Loops(a *App, entries []p2LoopEntry) ([]Loop, error) {
	seenEntry := make(map[string]bool, len(entries))
	seenLoop := map[string]bool{}
	var out []Loop
	for _, e := range entries {
		if e.name == "" || e.build == nil {
			return nil, fmt.Errorf("app: p2 loop entry %q: empty name or builder", e.name)
		}
		if seenEntry[e.name] {
			return nil, fmt.Errorf("app: p2 loop entry %q registered twice", e.name)
		}
		seenEntry[e.name] = true
		loops, err := e.build(a)
		if err != nil {
			return nil, fmt.Errorf("app: p2 loops %s: %w", e.name, err)
		}
		for _, l := range loops {
			if l.Name == "" || l.Run == nil {
				return nil, fmt.Errorf("app: p2 loops %s: loop %q has empty name or nil Run", e.name, l.Name)
			}
			if seenLoop[l.Name] {
				return nil, fmt.Errorf("app: p2 loops %s: duplicate loop name %q", e.name, l.Name)
			}
			seenLoop[l.Name] = true
			out = append(out, l)
		}
	}
	return out, nil
}
