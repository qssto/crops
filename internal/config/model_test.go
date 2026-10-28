package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const modelBase = `model:
  provider: openai
  prompt_version: v1
  prompts_dir: prompts
  default_tier: standard
  crop: rice
  region: jiangsu
  tiers:
    standard: {temperature: 0.1, max_tokens: 1024, timeout: 20s, max_retries: 1, rate_per_min: 60}
`

func TestLoadModel(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string { return writeFile(t, dir, name, body) }
	base := write("base.yaml", modelBase)
	tests := []struct {
		name      string
		file      string
		env       map[string]string
		wantErr   error
		errSubstr string
		check     func(t *testing.T, m *Model)
	}{
		{name: "ok", file: base, check: func(t *testing.T, m *Model) {
			tier := m.Tiers["standard"]
			if m.Provider != ModelProviderOpenAI || tier.Timeout != 20*time.Second || tier.MaxTokens != 1024 || tier.RatePerMin != 60 {
				t.Fatalf("model = %+v", m)
			}
		}},
		{name: "env selects stub", file: base, env: map[string]string{"AS_MODEL_PROVIDER": "stub"},
			check: func(t *testing.T, m *Model) {
				if m.Provider != ModelProviderStub {
					t.Fatalf("provider = %s", m.Provider)
				}
			}},
		{name: "api_key in yaml rejected", wantErr: ErrSecretInFile,
			file: write("secret.yaml", strings.Replace(modelBase, "  crop: rice\n", "  crop: rice\n  api_key: leaked\n", 1))},
		{name: "bad provider", errSubstr: "model.provider",
			file: write("provider.yaml", strings.Replace(modelBase, "provider: openai", "provider: magic", 1))},
		{name: "unknown default tier", errSubstr: "default_tier",
			file: write("tier.yaml", strings.Replace(modelBase, "default_tier: standard", "default_tier: fast", 1))},
		{name: "zero timeout", errSubstr: "timeout",
			file: write("timeout.yaml", strings.Replace(modelBase, "timeout: 20s", "timeout: 0s", 1))},
		{name: "missing region", errSubstr: "model.region",
			file: write("region.yaml", strings.Replace(modelBase, "  region: jiangsu\n", "", 1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AS_MODEL_PROVIDER", "") // 隔离宿主环境
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			m, err := LoadModel(tt.file)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.errSubstr != "":
				if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want mention of %q", err, tt.errSubstr)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				tt.check(t, m)
			}
		})
	}
}

func TestLoadModel_RepoConfig(t *testing.T) {
	t.Setenv("AS_MODEL_PROVIDER", "")
	m, err := LoadModel("../../configs/model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m.PromptVersion != "v1" || m.Region != "jiangsu" {
		t.Fatalf("model = %+v", m)
	}
}
