package evalkit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// ModelStub 是 Snapshot.Model 的桩模型取值（config.ModelProviderStub）；P0 评测固定用它。
const ModelStub = "stub"

// Snapshot 是实验配置快照（评测设计 §12.1、P0 §6.9 功能任务 4）：对比是否有效的前提。
type Snapshot struct {
	GitSHA string `json:"git_sha" yaml:"git_sha"`
	// GitDirty 为 true 表示运行时工作区有未提交改动（含 knowledge/），git_sha 不能完全复现本次结果。
	GitDirty      bool   `json:"git_dirty" yaml:"git_dirty"`
	PromptVersion string `json:"prompt_version" yaml:"prompt_version"`
	ModelTier     string `json:"model_tier" yaml:"model_tier"`
	// Model 是模型 provider（stub / openai）。
	Model string `json:"model" yaml:"model"`
	// KnowledgeRef 是所选样本 knowledge_ref 的去重列表（逗号分隔）；WORKTREE 指运行时工作区的 knowledge/。
	KnowledgeRef string `json:"knowledge_ref" yaml:"knowledge_ref"`
	// SamplesHash 是所选样本 sample_id@version 排序后的 sha256（§8.4-1 配对前提）。
	SamplesHash string `json:"samples_hash" yaml:"samples_hash"`
}

// IsZero 报告快照是否未填写。
func (s Snapshot) IsZero() bool { return s == Snapshot{} }

// SamplesHash 按 sample_id@version 排序后逐行求 sha256（十六进制）。
func SamplesHash(samples []*sample.Sample) string {
	ids := make([]string, 0, len(samples))
	for _, s := range samples {
		ids = append(ids, fmt.Sprintf("%s@%d", s.ID, s.Version))
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:])
}

// KnowledgeRefs 返回样本 knowledge_ref 的去重排序列表（逗号分隔）；空串按 WORKTREE 计。
func KnowledgeRefs(samples []*sample.Sample) string {
	set := map[string]struct{}{}
	for _, s := range samples {
		ref := s.KnowledgeRef
		if ref == "" {
			ref = knowledge.RefWorktree
		}
		set[ref] = struct{}{}
	}
	refs := make([]string, 0, len(set))
	for r := range set {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	return strings.Join(refs, ",")
}
