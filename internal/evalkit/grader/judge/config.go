package judge

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	qjudge "agri-steward/internal/service/quality/judge"
)

// 三个维度（路线 §6.3 功能任务 1），定义在 service/quality/judge（P2 L3 下沉，离线与线上同一份）。
const (
	DimFactConsistency    = qjudge.DimFactConsistency
	DimConclusionMatch    = qjudge.DimConclusionMatch
	DimExplanationQuality = qjudge.DimExplanationQuality
)

// Dimensions 是全部 judge 维度（报告与校准按此顺序）。
var Dimensions = qjudge.Dimensions

// 评分标准文件（eval/judges/<维度>/ 下）。
const (
	PromptFile    = qjudge.PromptFile
	RubricFile    = qjudge.RubricFile
	RecordingsDir = qjudge.RecordingsDir
	// DefaultConfig 是 judge 模型配置文件（与智能体 configs/model.yaml 分开）。
	DefaultConfig = "eval/judges/judge.yaml"
	// DefaultDir 是 judge 提示词 / 评分标准 / 录制的根目录。
	DefaultDir = qjudge.DefaultDir
	// ModelReplay 是录制回放客户端（不访问模型服务；待真实模型）。
	ModelReplay = qjudge.ModelReplay
	// versionLen 是 review_task.judge_version 列宽。
	versionLen = qjudge.VersionLen
)

var (
	// ErrUnknownDim 表示维度不在 Dimensions 内。
	ErrUnknownDim = qjudge.ErrUnknownDim
	// ErrBadConfig 表示 judge 配置缺项或取值不支持。
	ErrBadConfig = qjudge.ErrBadConfig
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
func CheckDim(dim string) error { return qjudge.CheckDim(dim) }

// Version 是 dim 的 judge 版本，见 qjudge.Version（≤ 32 字符，写 review_task.judge_version 与快照 judge_versions）。
func Version(dir, dim, tier string) (string, error) { return qjudge.Version(dir, dim, tier) }

// Versions 是全部维度的 judge 版本（快照 judge_versions 的取值）。
func Versions(dir, tier string) (map[string]string, error) { return qjudge.Versions(dir, tier) }
