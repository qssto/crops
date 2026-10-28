package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/config"
)

const repoRoot = "../.."

func testModel(provider string) *config.Model {
	return &config.Model{Provider: provider, PromptVersion: "v1", PromptsDir: repoRoot + "/prompts",
		DefaultTier: "standard", Crop: "rice", Region: "jiangsu",
		Tiers: map[string]config.ModelTier{"standard": {MaxTokens: 1024, Timeout: time.Second}}}
}

// NewAgent 工厂拿到已装配的仓储；主链路的 Meta 写入 AskService。
func TestBuild_NewAgentUsesAppRepos(t *testing.T) {
	tx, err := knowledge.LoadTaxonomy(repoRoot + "/eval/taxonomy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := fullDeps(t)
	d.Agent = nil
	var seen *App
	graphFactory := GraphAgent(context.Background(), GraphConfig{Model: testModel(config.ModelProviderStub),
		Chat: llm.Stub{}, KB: knowledge.NewStore(knowledge.NewRepoReader(repoRoot)), Taxonomy: tx})
	d.NewAgent = func(a *App) (agent.Agent, error) {
		seen = a
		return graphFactory(a)
	}
	a, err := Build(d)
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Runs == nil || seen.RunLocker == nil || a.Agent == nil {
		t.Fatalf("factory saw incomplete app: %+v", seen)
	}
	if _, ok := a.Agent.(metaer); !ok {
		t.Fatalf("agent %T has no Meta", a.Agent)
	}
}

func TestBuild_NewAgentError(t *testing.T) {
	d := fullDeps(t)
	d.Agent = nil
	d.NewAgent = GraphAgent(context.Background(), GraphConfig{})
	if _, err := Build(d); !errors.Is(err, ErrMissingGraphDeps) {
		t.Fatalf("err = %v, want ErrMissingGraphDeps", err)
	}
}

// openai 缺 ONEAPI_* 报清楚的错误，不退回桩；stub 只在显式选择时返回。
func TestChatModel_ProviderSwitch(t *testing.T) {
	for _, k := range []string{llm.EnvBaseURL, llm.EnvAPIKey, llm.EnvModel} {
		t.Setenv(k, "")
	}
	if _, err := ChatModel(testModel(config.ModelProviderOpenAI)); !errors.Is(err, llm.ErrMissingEnv) {
		t.Fatalf("openai without env: err = %v, want ErrMissingEnv", err)
	}
	m, err := ChatModel(testModel(config.ModelProviderStub))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(llm.Stub); !ok {
		t.Fatalf("stub provider returned %T", m)
	}
	t.Setenv(llm.EnvBaseURL, "http://127.0.0.1:1/v1")
	t.Setenv(llm.EnvAPIKey, "test-key-not-secret")
	t.Setenv(llm.EnvModel, "m")
	if m, err := ChatModel(testModel(config.ModelProviderOpenAI)); err != nil || m == nil {
		t.Fatalf("openai with env: %T, %v", m, err)
	}
	if _, err := ChatModel(testModel("bogus")); err == nil {
		t.Fatal("want error for unknown provider")
	}
}
