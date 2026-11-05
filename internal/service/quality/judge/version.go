// Package judge 是 LLM-judge 的公共部分（P2 L3 从 evalkit/grader/judge 下沉，调度方裁决 2）：维度与版本哈希、
// 提示词读取、材料组装与哈希、evidence 校验、录制回放客户端。离线评测（evalkit/grader/judge 包装成打分器）与
// 线上抽检（service/quality）共用，保证两边 judge_version 与材料哈希同一口径。本包不 import evalkit。
package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// DefaultDir 是 judge 提示词 / 评分标准 / 录制的根目录。
	DefaultDir = "eval/judges"
	// ModelReplay 是录制回放客户端（不访问模型服务；待真实模型）。
	ModelReplay = "replay"
	// VersionLen 是 judge_version 列宽（review_task / online_judge_result）。
	VersionLen = 32
	hashLen    = 16
)

var (
	// ErrUnknownDim 表示维度不在 Dimensions 内。
	ErrUnknownDim = errors.New("judge: unknown dimension")
	// ErrBadConfig 表示 judge 配置缺项或取值不支持。
	ErrBadConfig = errors.New("judge: bad config")
)

// CheckDim 报告 dim 是否为已知维度。
func CheckDim(dim string) error {
	if !slices.Contains(Dimensions, dim) {
		return fmt.Errorf("%w: %q", ErrUnknownDim, dim)
	}
	return nil
}

// Version 是 dim 的 judge 版本：sha256(prompt.md ‖ 0 ‖ rubric.md) 前 16 位 + "-" + 模型档位（≤ 32 字符，写 judge_version
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
	if len(v) > VersionLen {
		return "", fmt.Errorf("%w: judge version %q longer than %d (shorten model_tier)", ErrBadConfig, v, VersionLen)
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

// Prompt 是一次 judge 调用的提示词与评分标准（eval/judges/<维度>/ 原文）。
type Prompt struct {
	Dimension string
	Prompt    string
	Rubric    string
}

// LoadPrompt 读 dir/<dim>/{prompt.md,rubric.md}。
func LoadPrompt(dir, dim string) (Prompt, error) {
	if err := CheckDim(dim); err != nil {
		return Prompt{}, err
	}
	p := Prompt{Dimension: dim}
	for _, f := range []struct {
		name string
		dst  *string
	}{{PromptFile, &p.Prompt}, {RubricFile, &p.Rubric}} {
		b, err := os.ReadFile(filepath.Join(dir, dim, f.name))
		if err != nil {
			return Prompt{}, fmt.Errorf("judge: %s: %w", dim, err)
		}
		*f.dst = string(b)
	}
	return p, nil
}
