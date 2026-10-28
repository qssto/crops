package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// 模型提供方（configs/model.yaml 的 model.provider，可用 AS_MODEL_PROVIDER 覆盖）。
const (
	ModelProviderOpenAI = "openai" // OpenAI 兼容端点，读 env ONEAPI_BASE_URL / ONEAPI_API_KEY / ONEAPI_MODEL
	ModelProviderStub   = "stub"   // 确定性桩模型，必须显式选择
)

var modelProviders = map[string]bool{ModelProviderOpenAI: true, ModelProviderStub: true}

// modelSecretKeys 是不得出现在 yaml 中的模型密钥键。
var modelSecretKeys = []string{"model.api_key", "model.oneapi_api_key"}

// Model 是 configs/model.yaml：提供方、提示词版本、档位与默认情境。
type Model struct {
	Provider      string               `mapstructure:"provider"`
	PromptVersion string               `mapstructure:"prompt_version"`
	PromptsDir    string               `mapstructure:"prompts_dir"`
	DefaultTier   string               `mapstructure:"default_tier"`
	Crop          string               `mapstructure:"crop"`   // P0 只有水稻
	Region        string               `mapstructure:"region"` // 知识库区域 code（jiangsu）
	Backoff       time.Duration        `mapstructure:"backoff"`
	Tiers         map[string]ModelTier `mapstructure:"tiers"`
}

// ModelTier 是一个模型档位。
type ModelTier struct {
	Temperature float32       `mapstructure:"temperature"`
	MaxTokens   int           `mapstructure:"max_tokens"`
	Timeout     time.Duration `mapstructure:"timeout"`
	MaxRetries  int           `mapstructure:"max_retries"`
	RatePerMin  int           `mapstructure:"rate_per_min"` // 每分钟调用上限（Redis rl:model:{tier}），0 表示不限
}

// LoadModel 经 Viper 读取模型配置并校验，环境变量（前缀 AS_，如 AS_MODEL_PROVIDER=stub）覆盖 yaml。
func LoadModel(file string) (*Model, error) {
	v := viper.New()
	v.SetConfigFile(file)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", file, err)
	}
	for _, key := range modelSecretKeys {
		if v.InConfig(key) {
			return nil, fmt.Errorf("%w: %s in %s", ErrSecretInFile, key, file)
		}
	}
	var wrapper struct {
		Model Model `mapstructure:"model"`
	}
	if err := v.Unmarshal(&wrapper); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", file, err)
	}
	if err := wrapper.Model.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", file, err)
	}
	return &wrapper.Model, nil
}

func (m Model) validate() error {
	var errs []error
	if !modelProviders[m.Provider] {
		errs = append(errs, fmt.Errorf("model.provider %q not in [openai stub]", m.Provider))
	}
	for name, val := range map[string]string{"prompt_version": m.PromptVersion, "prompts_dir": m.PromptsDir,
		"crop": m.Crop, "region": m.Region} {
		if val == "" {
			errs = append(errs, fmt.Errorf("model.%s is required", name))
		}
	}
	if m.Backoff < 0 {
		errs = append(errs, errors.New("model.backoff must be >= 0"))
	}
	if len(m.Tiers) == 0 {
		errs = append(errs, errors.New("model.tiers needs at least one tier"))
	}
	if _, ok := m.Tiers[m.DefaultTier]; !ok {
		errs = append(errs, fmt.Errorf("model.default_tier %q is not a configured tier", m.DefaultTier))
	}
	for name, t := range m.Tiers {
		switch {
		case t.Timeout <= 0:
			errs = append(errs, fmt.Errorf("model.tiers.%s.timeout must be > 0", name))
		case t.MaxTokens <= 0:
			errs = append(errs, fmt.Errorf("model.tiers.%s.max_tokens must be > 0", name))
		case t.MaxRetries < 0 || t.RatePerMin < 0:
			errs = append(errs, fmt.Errorf("model.tiers.%s.max_retries and rate_per_min must be >= 0", name))
		case t.Temperature < 0 || t.Temperature > 2:
			errs = append(errs, fmt.Errorf("model.tiers.%s.temperature must be in [0, 2]", name))
		}
	}
	return errors.Join(errs...)
}
