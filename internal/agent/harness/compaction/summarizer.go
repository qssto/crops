package compaction

import (
	"context"
	"fmt"
	"strings"
)

// DeterministicPromptVersion 是确定性摘要器的 prompt_version（stub 下使用；真实轻量档模型摘要待真实模型）。
const DeterministicPromptVersion = "deterministic-v1"

// Summarizer 把更早的自由文本轮次压成摘要（只压自由文本，白名单事实不经过它）。
// prev 为已有摘要（可空），turns 为本次新压缩的轮次；返回文本的估算 token 须 ≤ maxTokens。
type Summarizer interface {
	Summarize(ctx context.Context, prev string, turns []Turn, maxTokens int) (text, promptVersion string, err error)
}

// Deterministic 是确定性摘要器：已有摘要 + 每轮「[step] 问：… 答：…」，从尾部按字符截断到 maxTokens 以内。
// 截断只影响自由文本摘要；被截掉的约束 / 结论等白名单内容仍在 facts 中，原文经 raw_refs 可回放。
type Deterministic struct{}

var _ Summarizer = Deterministic{}

// Summarize 实现 Summarizer。
func (Deterministic) Summarize(_ context.Context, prev string, turns []Turn, maxTokens int) (string, string, error) {
	if maxTokens <= 0 {
		return "", "", fmt.Errorf("compaction: summary max tokens %d", maxTokens)
	}
	var b strings.Builder
	b.WriteString(prev)
	for _, t := range turns {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[%s] 问：%s", t.StepID, t.Question)
		if t.Answer != "" {
			fmt.Fprintf(&b, " 答：%s", t.Answer)
		}
	}
	return truncateTokens(b.String(), maxTokens), DeterministicPromptVersion, nil
}

// truncateTokens 按字符截断到估算 token ≤ max（保留开头）。
func truncateTokens(s string, max int) string {
	if EstimateTokens(s) <= max {
		return s
	}
	r := []rune(s)
	lo, hi := 0, len(r)
	for lo < hi { // 二分最长的合格前缀
		mid := (lo + hi + 1) / 2
		if EstimateTokens(string(r[:mid])) <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(r[:lo])
}
