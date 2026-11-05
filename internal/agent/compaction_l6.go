package agent

import (
	"context"
	"sync"

	"agri-steward/internal/model"
)

// ---- P2 L6：压缩状态随中断事务 / 终态写入 agent_run 的 ctx 载体（P2 §6.7 技术实现 6）----

// CompactionCarrier 在一次 run 的执行 ctx 里携带待持久化的压缩状态：图在上下文组装后 Set，
// CheckpointStore.Set（中断事务）与 RunStateStore.FinishRun（终态）读到非空状态时一并写
// agent_run.compaction_state / compaction_count。没有载体或载体为空时两者的 SQL 与 P1 相同。可并发使用。
type CompactionCarrier struct {
	mu    sync.Mutex
	state *model.CompactionState
	count int
}

type compactionCarrierKey struct{}

// WithCompactionCarrier 返回带新载体的 ctx 与载体本身（graph 在 runner.Run / Resume 之前调用）。
func WithCompactionCarrier(ctx context.Context) (context.Context, *CompactionCarrier) {
	c := &CompactionCarrier{}
	return context.WithValue(ctx, compactionCarrierKey{}, c), c
}

// CompactionCarrierFrom 取 ctx 中的载体；没有返回 nil。
func CompactionCarrierFrom(ctx context.Context) *CompactionCarrier {
	c, _ := ctx.Value(compactionCarrierKey{}).(*CompactionCarrier)
	return c
}

// Set 记下本 run 使用的压缩状态与本 run 内已做的压缩次数（state 为 nil 时清空）。
func (c *CompactionCarrier) Set(state *model.CompactionState, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state, c.count = state, count
}

// Get 返回压缩状态与次数；ok 为 false 表示本 run 未使用压缩状态（持久化走 P1 SQL）。
func (c *CompactionCarrier) Get() (state *model.CompactionState, count int, ok bool) {
	if c == nil {
		return nil, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.count, c.state != nil
}
