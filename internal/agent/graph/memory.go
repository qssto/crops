package graph

import (
	"context"

	"agri-steward/internal/agent"
)

// 记忆注入（P0 §6.7 功能任务 6）：explain 组装模型入参时追加近期记忆摘要；Config.Memory 为空或开关关闭即跳过。
const (
	memoryLimit  = 5
	memoryPrefix = "历史记录："
)

// memoryHints 读该地块的注入记忆并转为无数字的依据摘要（数值只能来自规则层）；读失败只告警，不影响主链路。
func (a *Agent) memoryHints(ctx context.Context, s *state) []string {
	if a.cfg.Memory == nil {
		return nil
	}
	items, err := a.cfg.Memory.Recent(ctx, s.q.Scope, s.q.FieldID, memoryLimit)
	if err != nil {
		a.log.WarnContext(ctx, "memory unavailable", "run_id", s.runID, "err", err)
		return nil
	}
	return hintsOf(items)
}

// hintsOf 把记忆条目转为模型可见的摘要；去数字后为空的条目丢弃。
func hintsOf(items []agent.MemoryItem) []string {
	var out []string
	for _, it := range items {
		if txt := noDigits(it.Summary); txt != "" {
			out = append(out, memoryPrefix+txt)
		}
	}
	return out
}
