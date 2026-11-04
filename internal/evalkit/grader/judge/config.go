package judge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// 三个维度（路线 §6.3 功能任务 1）。
const (
	DimFactConsistency    = "fact_consistency"
	DimConclusionMatch    = "conclusion_match"
	DimExplanationQuality = "explanation_quality"
)

// Dimensions 是全部 judge 维度（报告与校准按此顺序）。
var Dimensions = []string{DimFactConsistency, DimConclusionMatch, DimExplanationQuality}

// 评分标准文件（eval/judges/<维度>/ 下）。
const (
	PromptFile    = "prompt.md"
	RubricFile    = "rubric.md"
	RecordingsDir = "recordings"
	// DefaultConfig 是 judge 模型配置文件（与智能体 configs/model.yaml 分开）。
	DefaultConfig = "eval/judges/judge.yaml"
	// DefaultDir 是 judge 提示词 / 评分标准 / 录制的根目录。
	DefaultDir = "eval/judges"
	// ModelReplay 是录制回放客户端（不访问模型服务；待真实模型）。
	ModelReplay = "replay"
	// versionLen 是 review_task.judge_version 列宽。
	versionLen = 32
	hashLen    = 16
)

var (
	// ErrUnknownDim 表示维度不在 Dimensions 内。
	ErrUnknownDim = errors.New("judge: unknown dimension")
	// ErrBadConfig 表示 judge 配置缺项或取值不支持。
	ErrBadConfig = errors.New("judge: bad config")
)

// Config 是 judge 模型配置（eval.judge.model / eval.judge.model_tier）。model 现只支持 replay；
// model_tier 进 judge_version，换档位视为打分器变更、须重新校准。
type Config struct {
	Model     string `yaml:"model"`
	ModelTier string `yaml:"model_tier"`
}

type configFile struct {
	Eval struct {
		Judge Config `yaml:"judge"`
	} `yaml:"eval"`
}

// LoadConfig 严格解析 judge 配置（未知字段报错）。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("judge: read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var f configFile
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("judge: parse %s: %w", path, err)
	}
	c := f.Eval.Judge
	if c.Model != ModelReplay {
		return Config{}, fmt.Errorf("%w: eval.judge.model %q (only %q until a real judge model is approved)", ErrBadConfig, c.Model, ModelReplay)
	}
	if c.ModelTier == "" {
		return Config{}, fmt.Errorf("%w: eval.judge.model_tier is empty", ErrBadConfig)
	}
	return c, nil
}

// CheckDim 报告 dim 是否为已知维度。
func CheckDim(dim string) error {
	if !slices.Contains(Dimensions, dim) {
		return fmt.Errorf("%w: %q", ErrUnknownDim, dim)
	}
	return nil
}

// Version 是 dim 的 judge 版本：sha256(prompt.md ‖ 0 ‖ rubric.md) 前 16 位 + "-" + 模型档位（≤ 32 字符，写 review_task.judge_version
// 与快照 judge_versions）。提示词、评分标准或档位任一变更，版本即变。
func Version(dir, dim, tier string) (string, error) {
	if err := CheckDim(dim); err != nil {
		return "", err
	}
	h := sha256.New()
	for i, name := range []string{PromptFile, RubricFile} {
		b, err := os.ReadFile(filepath.Join(dir, dim, name))
		if err != nil {
			return "", fmt.Errorf("judge: %s: %w", dim, err)
		}
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write(b)
	}
	v := hex.EncodeToString(h.Sum(nil))[:hashLen] + "-" + tier
	if len(v) > versionLen {
		return "", fmt.Errorf("%w: judge version %q longer than %d (shorten model_tier)", ErrBadConfig, v, versionLen)
	}
	return v, nil
}

// Versions 是全部维度的 judge 版本（快照 judge_versions 的取值）。
func Versions(dir, tier string) (map[string]string, error) {
	out := make(map[string]string, len(Dimensions))
	for _, d := range Dimensions {
		v, err := Version(dir, d, tier)
		if err != nil {
			return nil, err
		}
		out[d] = v
	}
	return out, nil
}
