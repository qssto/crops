package app

import (
	"context"
	"errors"
	"fmt"

	einomodel "github.com/cloudwego/eino/components/model"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/config"
)

// GraphConfig 是装配建议主链路所需的模型与知识依赖（仓储取自 App）。
type GraphConfig struct {
	Model    *config.Model           // configs/model.yaml：提示词、档位、作物 / 区域
	Chat     einomodel.BaseChatModel // 底层模型：ChatModel(...) 的结果，或评测里显式的 llm.Stub{}
	KB       *knowledge.Store
	Taxonomy *knowledge.Taxonomy
}

// ErrMissingGraphDeps 表示 GraphConfig 缺字段。
var ErrMissingGraphDeps = errors.New("app: graph needs Model, Chat, KB and Taxonomy")

// GraphAgent 返回 Deps.NewAgent 工厂：用 App 已装配的 Tools / Runs / Advice / Drafts / RunLocker / Clock /
// Approvals（审批闸门）/ Events 构造主链路。
func GraphAgent(ctx context.Context, gc GraphConfig) func(*App) (agent.Agent, error) {
	return func(a *App) (agent.Agent, error) {
		if gc.Model == nil || gc.Chat == nil || gc.KB == nil || gc.Taxonomy == nil {
			return nil, ErrMissingGraphDeps
		}
		m := gc.Model
		prompts, err := llm.LoadPrompts(m.PromptsDir, m.PromptVersion)
		if err != nil {
			return nil, err
		}
		tiers := make(map[llm.Tier]llm.TierConfig, len(m.Tiers))
		for name, t := range m.Tiers {
			tiers[llm.Tier(name)] = llm.TierConfig{Temperature: t.Temperature, MaxTokens: t.MaxTokens,
				Timeout: t.Timeout, MaxRetries: t.MaxRetries}
		}
		c, err := llm.New(llm.Config{Model: gc.Chat, Prompts: prompts, Tiers: tiers, Backoff: m.Backoff})
		if err != nil {
			return nil, err
		}
		cfg := graph.Config{LLM: c, Tier: llm.Tier(m.DefaultTier), Tools: a.Tools,
			Runs: a.Runs, Advice: a.Advice, Drafts: a.Drafts, Locker: a.RunLocker, Clock: a.Clock,
			KB: gc.KB, Taxonomy: gc.Taxonomy, Crop: m.Crop, Region: m.Region, Logger: a.Logger,
			Gate: a.Approvals, Events: a.Events}
		a.HITL.withHITL(&cfg, a.Runs)
		return graph.New(ctx, cfg)
	}
}

// ChatModel 按 model.provider 选底层模型：openai 读 env ONEAPI_BASE_URL / ONEAPI_API_KEY / ONEAPI_MODEL，
// 缺失时报错（不静默退回桩）；stub 只在显式选择时使用。
func ChatModel(m *config.Model) (einomodel.BaseChatModel, error) {
	switch m.Provider {
	case config.ModelProviderOpenAI:
		oc, err := llm.OpenAIConfigFromEnv()
		if err != nil {
			return nil, fmt.Errorf("app: model.provider=openai: %w（或显式设置 AS_MODEL_PROVIDER=stub 使用桩模型）", err)
		}
		return llm.NewOpenAI(oc)
	case config.ModelProviderStub:
		return llm.Stub{}, nil
	}
	return nil, fmt.Errorf("app: unknown model.provider %q", m.Provider)
}
