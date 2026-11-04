package evalkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// KnowledgeContentPrefix 是 knowledge_ref 内容哈希的前缀（kc:<sha256 十六进制>）。
const KnowledgeContentPrefix = "kc:"

// KnowledgeContentRef 求 ref 下 knowledge/ 内容的确定性哈希：文件按相对路径排序，逐个写入
// 「路径 \x00 字节数 \x00 内容」后求 sha256。隐藏文件（任一路径段以 . 开头）不计入。
// r 须是 knowledge.NewTrackedRepoReader（P2-L0 第二轮审查修复）：ref 为 WORKTREE 时只算 git 已跟踪文件、内容取磁盘
// （未提交改动如实反映，未跟踪的备份 / 新文件不计入），否则经 git 读该提交；两种读法遇符号链接都报错，
// 同一内容算出同一哈希。用 knowledge.NewRepoReader 时工作区侧会把未跟踪文件也算进去，口径不一致。
func KnowledgeContentRef(ctx context.Context, r knowledge.TreeReader, ref string) (string, error) {
	files, err := r.List(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("knowledge_ref %s: %w", ref, err)
	}
	sort.Strings(files)
	h := sha256.New()
	counted := 0
	for _, p := range files {
		if hiddenPath(p) {
			continue
		}
		b, err := r.ReadFile(ctx, ref, p)
		if err != nil {
			return "", fmt.Errorf("knowledge_ref %s: %w", ref, err)
		}
		h.Write([]byte(p + "\x00" + strconv.Itoa(len(b)) + "\x00"))
		h.Write(b)
		counted++
	}
	if counted == 0 {
		return "", fmt.Errorf("knowledge_ref %s: no knowledge files", ref)
	}
	return KnowledgeContentPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// KnowledgeRef 是快照 knowledge_ref：样本 knowledge_ref 规整去重后逐个求内容哈希，排序逗号拼接。
// worktreeRef 是样本写 WORKTREE（或空）时实际读取的 ref：运行时为 WORKTREE；补录 P1 实验时为该实验的 git_sha
// （按提交内容近似当时的工作区）。
func KnowledgeRef(ctx context.Context, r knowledge.TreeReader, samples []*sample.Sample, worktreeRef string) (string, error) {
	refs := map[string]struct{}{}
	for _, s := range samples {
		ref, err := knowledge.NormalizeRef(s.KnowledgeRef)
		if err != nil {
			return "", fmt.Errorf("sample %s: %w", s.ID, err)
		}
		if ref == knowledge.RefWorktree {
			ref = worktreeRef
		}
		refs[ref] = struct{}{}
	}
	hashes := map[string]struct{}{}
	for ref := range refs {
		h, err := KnowledgeContentRef(ctx, r, ref)
		if err != nil {
			return "", err
		}
		hashes[h] = struct{}{}
	}
	out := make([]string, 0, len(hashes))
	for h := range hashes {
		out = append(out, h)
	}
	if len(out) == 0 {
		return "", errors.New("knowledge_ref: no samples")
	}
	sort.Strings(out)
	return strings.Join(out, ","), nil
}

func hiddenPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}
