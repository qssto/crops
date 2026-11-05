package usersim

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultConfig 是模拟用户模型配置（与被测 agent 的 configs/model.yaml、judge 的 eval/judges/judge.yaml 分开），相对仓库根。
	DefaultConfig = "eval/usersim/usersim.yaml"
	// DefaultRecordingsDir 是录制目录（{sample_id}.yaml），相对仓库根。
	DefaultRecordingsDir = "eval/usersim/recordings"
	// ModelReplay 是录制回放客户端（不访问模型服务；待真实模型）。
	ModelReplay = "replay"
)

// ErrBadConfig 表示模拟用户配置缺项或取值不支持。
var ErrBadConfig = errors.New("usersim: bad config")

// Config 是模拟用户模型配置（eval.usersim.model / eval.usersim.model_tier）。model 现只支持 replay。
type Config struct {
	Model     string `yaml:"model"`
	ModelTier string `yaml:"model_tier"`
}

type configFile struct {
	Eval struct {
		UserSim Config `yaml:"usersim"`
	} `yaml:"eval"`
}

// LoadConfig 严格解析模拟用户配置（未知字段报错）。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("usersim: read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f configFile
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("usersim: parse %s: %w", path, err)
	}
	c := f.Eval.UserSim
	if c.Model != ModelReplay {
		return Config{}, fmt.Errorf("%w: eval.usersim.model %q (only %q until a real user-sim model is approved)", ErrBadConfig, c.Model, ModelReplay)
	}
	if c.ModelTier == "" {
		return Config{}, fmt.Errorf("%w: eval.usersim.model_tier is empty", ErrBadConfig)
	}
	return c, nil
}

// Load 读配置并装配模拟用户通道（现只有录制回放）。
func Load(configPath, recordingsDir string) (*Simulated, Config, error) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, Config{}, err
	}
	rp, err := LoadReplay(recordingsDir)
	if err != nil {
		return nil, Config{}, err
	}
	return NewSimulated(rp), cfg, nil
}
