// Package compare 是 P2 L0 的实验对比（评测设计 §8.4、P2 §6.1）：可比因子注册表与差异检查、
// (sample_id, sample_version) 交集上的样本级配对 bootstrap、单因子 / 阶段级结论与报告、A/A 噪声阈值、候选因子登记表。
package compare

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"agri-steward/internal/evalkit"
)

// Factor 是一个可比因子：名字 → 从快照取值。git_sha / git_dirty / samples_hash 与运行参数、结果
// （suites / n / token_used / cost / wall_time）不是因子。L4 的 rule_offload.*、L6 的 enable_compaction 只加注册项。
type Factor struct {
	Name  string
	Value func(evalkit.Snapshot) string
	// Pinned 为 true 时单因子模式下不一致直接拒绝（提示重建 baseline），不计入差异因子。
	Pinned bool
	// PhasePinned 为 true 时阶段级模式下也必须一致。
	PhasePinned bool
}

// Factors 是可比因子注册表（顺序即报告顺序）。
var Factors = []Factor{
	{Name: "prompt_version", Value: func(s evalkit.Snapshot) string { return s.PromptVersion }},
	{Name: "model_tier", Value: func(s evalkit.Snapshot) string { return s.ModelTier }},
	{Name: "model", Value: func(s evalkit.Snapshot) string { return s.Model }},
	{Name: "loader_strategy", Value: func(s evalkit.Snapshot) string { return s.LoaderStrategy }},
	{Name: "retrieval_strategy", Value: func(s evalkit.Snapshot) string { return s.RetrievalStrategy }},
	{Name: "knowledge_ref", Value: func(s evalkit.Snapshot) string { return s.KnowledgeRef }, Pinned: true},
	{Name: "judge_versions", Value: evalkit.Snapshot.JudgeVersionsKey, Pinned: true, PhasePinned: true},
}

// FactorDiff 是一个不一致的因子（写 experiment.factor_diff：[{name, baseline, candidate}]）。
type FactorDiff struct {
	Name      string `json:"name"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
}

// Diff 按注册表逐项比对两份快照，返回全部不一致的因子（含 Pinned 项）。
func Diff(factors []Factor, baseline, candidate evalkit.Snapshot) []FactorDiff {
	var out []FactorDiff
	for _, f := range factors {
		b, c := f.Value(baseline), f.Value(candidate)
		if b != c {
			out = append(out, FactorDiff{Name: f.Name, Baseline: b, Candidate: c})
		}
	}
	return out
}

// Mode 是对比模式。
type Mode string

// 对比模式。
const (
	// ModeFactor 是单因子模式（默认，PR 与发版门禁）。
	ModeFactor Mode = "factor"
	// ModePhase 是阶段级模式（只用于 Gate 2 第 3 条，只出整体结论）。
	ModePhase Mode = "phase"
)

// ParseMode 解析 --mode。
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeFactor, ModePhase:
		return m, nil
	}
	return "", fmt.Errorf("compare: --mode must be factor or phase, got %q", s)
}

var (
	// ErrIncomparable 表示两边快照不满足对比前提（单因子：knowledge_ref / judge_versions；阶段级：samples_hash / judge_versions；
	// 两种模式：运行参数 n / suites）。
	ErrIncomparable = errors.New("compare: snapshots are not comparable, rebuild the baseline (评测设计 §8.4-4)")
	// ErrNoSnapshot 表示一边实验没有快照或快照有空字段（P1 实验须先 baseline freeze --backfill）。
	ErrNoSnapshot = errors.New("compare: experiment has no complete snapshot")
)

// RunParam 是可比性前提里的运行参数：不是因子、不进因子计数，但不一致时两边不可比（直接拒绝）。
type RunParam struct {
	Name  string
	Value func(evalkit.Snapshot) string
	// Factor / Phase 为 true 时该模式要求一致。
	Factor, Phase bool
}

// RunParams 是运行参数注册表（P2-L0 审查修复）：n、suites、tags 两种模式都要求一致；samples_hash 只在阶段级要求
// （单因子模式按 (sample_id, sample_version) 交集配对，允许样本清单不同；baseline 有而 candidate 未配对的样本另判 INCOMPLETE）。
// suites 在阶段级也要求一致：samples_hash 已锁样本清单，但按套件的红线归约（pass^k）依赖 suites 标签，拿不准按失败关闭处理。
// tags（样本筛选条件，第二轮审查修复）同理：candidate 用 --tags 只跑一部分样本时直接不可比。
var RunParams = []RunParam{
	{Name: "n", Value: func(s evalkit.Snapshot) string { return strconv.Itoa(s.N) }, Factor: true, Phase: true},
	{Name: "suites", Value: func(s evalkit.Snapshot) string { return listKey(s.Suites) }, Factor: true, Phase: true},
	{Name: "tags", Value: func(s evalkit.Snapshot) string { return listKey(s.Tags) }, Factor: true, Phase: true},
	{Name: "samples_hash", Value: func(s evalkit.Snapshot) string { return s.SamplesHash }, Phase: true},
}

// listKey 是 suites / tags 的规范文本（排序后逗号拼接，顺序不影响可比性；元素非空由 RequireComplete 保证）。
func listKey(items []string) string {
	xs := slices.Clone(items)
	slices.Sort(xs)
	return strings.Join(xs, ",")
}

func (p RunParam) required(mode Mode) bool {
	return (mode == ModeFactor && p.Factor) || (mode == ModePhase && p.Phase)
}

func (f Factor) pinned(mode Mode) bool {
	return (mode == ModeFactor && f.Pinned) || (mode == ModePhase && f.PhasePinned)
}

// RequireComplete 要求快照无空字段（Snapshot.Missing 为空）且格式正确（Snapshot.Malformed 为空：knowledge_ref 每段 kc:、
// suites / tags 元素非空）；JSON null 与 {} 解出的零值快照、旧格式快照（knowledge_ref = WORKTREE、无 tags）都在此被拒绝。
func RequireComplete(side string, s evalkit.Snapshot) error {
	var problems []string
	if missing := s.Missing(); len(missing) > 0 {
		problems = append(problems, "missing "+strings.Join(missing, ","))
	}
	if bad := s.Malformed(); len(bad) > 0 {
		problems = append(problems, "malformed "+strings.Join(bad, ","))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s %s", ErrNoSnapshot, side, strings.Join(problems, "; "))
	}
	return nil
}

// Check 校验对比前提（失败关闭）：两边快照完整、运行参数一致、Pinned 因子一致；返回参与归因的差异因子（不含 Pinned 项）。
func Check(factors []Factor, mode Mode, baseline, candidate evalkit.Snapshot) ([]FactorDiff, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return nil, err
	}
	if err := errors.Join(RequireComplete(sideBaseline, baseline), RequireComplete(sideCandidate, candidate)); err != nil {
		return nil, err
	}
	for _, p := range RunParams {
		if b, c := p.Value(baseline), p.Value(candidate); p.required(mode) && b != c {
			return nil, fmt.Errorf("%w: %s %q != %q", ErrIncomparable, p.Name, b, c)
		}
	}
	pinned := map[string]bool{}
	for _, f := range factors {
		pinned[f.Name] = f.pinned(mode)
	}
	var out []FactorDiff
	for _, d := range Diff(factors, baseline, candidate) {
		if pinned[d.Name] {
			return nil, fmt.Errorf("%w: %s %q != %q", ErrIncomparable, d.Name, d.Baseline, d.Candidate)
		}
		out = append(out, d)
	}
	return out, nil
}
