package app

import "agri-steward/internal/agent"

// runUsage 是主链路写 agent_run P2 列的接缝（graph.Config.Usage，P2 第 3 批）；Runs 未装配时返回 nil 接口
// （不是带空指针的接口），主链路不写。
func (a *App) runUsage() agent.RunUsageStore {
	if a.Runs == nil {
		return nil
	}
	return a.Runs
}
