package model

// RunUsage 是 agent_run 的 P2 列（迁移 0014_agent_run_p2，第 3 批接缝）：L5 写 LoaderStrategy，
// L6 写 token 累计 / 压缩次数 / 会话链。迁移前的行与未开开关的 run 取列默认值（v0 / 0 / NULL）。
type RunUsage struct {
	LoaderStrategy   string  `json:"loader_strategy"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CompactionCount  int     `json:"compaction_count"`
	PrevRunID        *uint64 `json:"prev_run_id,omitempty"`
}

// agent_run.loader_strategy 取值（P2 L5；列默认 v0）。
const (
	LoaderStrategyV0 = "v0"
	LoaderStrategyV1 = "v1"
)
