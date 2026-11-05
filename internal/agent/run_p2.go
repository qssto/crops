package agent

import (
	"context"

	"agri-steward/internal/model"
)

// ---- P2 第 3 批：agent_run P2 列写入接缝（迁移 0014_agent_run_p2）----

// RunUsageStore 写 agent_run 的 P2 列，不改状态、不升 row_ver（与心跳同类，不和 FinishRun 的乐观锁冲突）。
// 实现：repository.RunRepo。L5 调 SetLoaderStrategy，L6 调 AddTokens；L6 的 compaction_count / prev_run_id 在 L6 自己的新文件里加方法。
// 写入失败只记日志，不让 run 失败（成本 / 诊断统计，不是业务状态）。
type RunUsageStore interface {
	SetLoaderStrategy(ctx context.Context, scope model.Scope, runID uint64, strategy string) error
	AddTokens(ctx context.Context, scope model.Scope, runID uint64, prompt, completion int) error
}
