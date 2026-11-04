package evalkit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"agri-steward/internal/evalkit/sample"
)

// ModelStub 是 Snapshot.Model 的桩模型取值（config.ModelProviderStub）；P0 评测固定用它。
const ModelStub = "stub"

// 加载 / 检索策略的当前取值（P2 L0 快照字段；L4 起 loader 有新版本时在此加常量）。
const (
	LoaderV0             = "v0"
	RetrievalProgressive = "progressive"
)

// Snapshot 是实验配置快照（评测设计 §12.1、P0 §6.9 功能任务 4、P2 §6.1）：对比是否有效的前提。
// 前半段是配置（Start 时写入），Suites 起是本次运行的规模与结果（Finish 回填）。
type Snapshot struct {
	GitSHA string `json:"git_sha" yaml:"git_sha"`
	// GitDirty 为 true 表示运行时工作区有未提交改动（含 knowledge/），git_sha 不能完全复现本次结果。
	GitDirty      bool   `json:"git_dirty" yaml:"git_dirty"`
	PromptVersion string `json:"prompt_version" yaml:"prompt_version"`
	ModelTier     string `json:"model_tier" yaml:"model_tier"`
	// Model 是模型 provider（stub / openai）。
	Model string `json:"model" yaml:"model"`
	// KnowledgeRef 是所选样本所读知识的内容哈希（kc:<sha256>，见 KnowledgeRef()）；多个 ref 时排序逗号拼接。
	KnowledgeRef string `json:"knowledge_ref" yaml:"knowledge_ref"`
	// SamplesHash 是所选样本 sample_id@version 排序后的 sha256（§8.4-1 配对前提）。
	SamplesHash       string `json:"samples_hash" yaml:"samples_hash"`
	LoaderStrategy    string `json:"loader_strategy" yaml:"loader_strategy"`
	RetrievalStrategy string `json:"retrieval_strategy" yaml:"retrieval_strategy"`
	// JudgeVersions 是 维度 → judge 版本；空 map 表示未接 judge（非 nil，序列化为 {}）。
	JudgeVersions map[string]string `json:"judge_versions" yaml:"judge_versions"`

	Suites []string `json:"suites" yaml:"suites"`
	// Tags 是本次运行的样本筛选条件（cmd/eval run --tags，带任一 tag 的样本）：空表（非 nil，序列化为 []）表示不按 tag 筛选，
	// nil 表示快照没记录（P2-L0 第二轮审查修复前的实验），按缺字段处理。运行参数，不是因子。
	Tags []string `json:"tags" yaml:"tags"`
	N    int      `json:"n" yaml:"n"`
	// TokenUsed / Cost 是全部 trial 的 token 合计与费用；桩模型为 0。
	TokenUsed int     `json:"token_used" yaml:"token_used"`
	Cost      float64 `json:"cost" yaml:"cost"`
	// WallTime 是 started_at 到 finished_at 的墙钟时长（time.Duration 文本，秒级）。
	WallTime string `json:"wall_time" yaml:"wall_time"`
	// VoidInfraErrorRate 是本次 run 判作废实际使用的 infra_error 占比阈值（门禁参数，不是因子）：resume 一律沿用此值，
	// 不读当时配置；0 表示快照没记录（L7-a 评审修复前的实验），resume 失败关闭。
	VoidInfraErrorRate float64 `json:"void_infra_error_rate,omitempty" yaml:"void_infra_error_rate,omitempty"`
}

// IsZero 报告快照是否未填写（P1 及以前的实验 snapshot 为 NULL）。
func (s Snapshot) IsZero() bool {
	return s.GitSHA == "" && s.PromptVersion == "" && s.ModelTier == "" && s.Model == "" &&
		s.KnowledgeRef == "" && s.SamplesHash == "" && s.LoaderStrategy == "" && s.RetrievalStrategy == "" &&
		s.JudgeVersions == nil && len(s.Suites) == 0 && s.Tags == nil && s.N == 0 && s.WallTime == ""
}

// MissingConfig 返回未填写的配置字段名（Start 前必须为空列表）；judge_versions 只要求非 nil。
func (s Snapshot) MissingConfig() []string {
	fields := []struct {
		name  string
		empty bool
	}{
		{"git_sha", s.GitSHA == ""},
		{"prompt_version", s.PromptVersion == ""},
		{"model_tier", s.ModelTier == ""},
		{"model", s.Model == ""},
		{"knowledge_ref", s.KnowledgeRef == ""},
		{"samples_hash", s.SamplesHash == ""},
		{"loader_strategy", s.LoaderStrategy == ""},
		{"retrieval_strategy", s.RetrievalStrategy == ""},
		{"judge_versions", s.JudgeVersions == nil},
		{"suites", len(s.Suites) == 0},
		{"tags", s.Tags == nil},
		{"n", s.N < 1},
	}
	var missing []string
	for _, f := range fields {
		if f.empty {
			missing = append(missing, f.name)
		}
	}
	return missing
}

// Missing 返回未填写的全部字段名（配置 + 结果；冻结前必须为空列表）。token_used / cost 为 0 合法（桩模型）。
func (s Snapshot) Missing() []string {
	missing := s.MissingConfig()
	if s.WallTime == "" {
		missing = append(missing, "wall_time")
	}
	return missing
}

// snapshotFormats 是快照字段的格式规则（P2-L0 第二轮审查修复）：bad 为 true 即该字段格式不对。新规则只加注册项。
var snapshotFormats = []struct {
	name string
	bad  func(Snapshot) bool
}{
	// knowledge_ref 每段（逗号分隔）都须是 kc:<哈希>；旧格式 WORKTREE / 提交号不是内容哈希，不能据此判可比。
	{"knowledge_ref", func(s Snapshot) bool {
		return slices.ContainsFunc(strings.Split(s.KnowledgeRef, ","), func(seg string) bool {
			h, ok := strings.CutPrefix(seg, KnowledgeContentPrefix)
			return !ok || h == ""
		})
	}},
	{"suites", func(s Snapshot) bool { return slices.ContainsFunc(s.Suites, blank) }},
	{"tags", func(s Snapshot) bool { return slices.ContainsFunc(s.Tags, blank) }},
}

func blank(v string) bool { return strings.TrimSpace(v) == "" }

// Malformed 返回已填写但格式不对的字段名（未填写的字段由 Missing 报告，这里不重复）。
func (s Snapshot) Malformed() []string {
	missing := map[string]bool{}
	for _, m := range s.Missing() {
		missing[m] = true
	}
	var bad []string
	for _, f := range snapshotFormats {
		if !missing[f.name] && f.bad(s) {
			bad = append(bad, f.name)
		}
	}
	return bad
}

// JudgeVersionsKey 是 judge_versions 的规范文本：按维度排序，维度与版本各自 strconv.Quote 转义后写成 "dim"="version"，
// 逗号分隔（转义保证不同映射不会拼出同一文本）；未接 judge 为空串。
func (s Snapshot) JudgeVersionsKey() string {
	dims := make([]string, 0, len(s.JudgeVersions))
	for dim := range s.JudgeVersions {
		dims = append(dims, dim)
	}
	sort.Strings(dims)
	pairs := make([]string, 0, len(dims))
	for _, dim := range dims {
		pairs = append(pairs, strconv.Quote(dim)+"="+strconv.Quote(s.JudgeVersions[dim]))
	}
	return strings.Join(pairs, ",")
}

// SampleRefs 返回样本 sample_id@version 的排序列表（SamplesHash 与 manifest 共用）。
func SampleRefs(samples []*sample.Sample) []string {
	ids := make([]string, 0, len(samples))
	for _, s := range samples {
		ids = append(ids, fmt.Sprintf("%s@%d", s.ID, s.Version))
	}
	sort.Strings(ids)
	return ids
}

// SamplesHash 按 sample_id@version 排序后逐行求 sha256（十六进制）。
func SamplesHash(samples []*sample.Sample) string {
	sum := sha256.Sum256([]byte(strings.Join(SampleRefs(samples), "\n")))
	return hex.EncodeToString(sum[:])
}
