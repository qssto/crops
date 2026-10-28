package knowledge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// RefWorktree 表示读工作区目录（不经 git），空 ref 与之等价。
const RefWorktree = "WORKTREE"

// sampleRefPrefix 是样本 knowledge_ref 的提交号写法前缀：kb@<sha>。
const sampleRefPrefix = "kb@"

// knowledgeDir 是知识库在仓库中的目录。
const knowledgeDir = "knowledge"

// gitRefPattern 限定可传给 git 的 ref：不得以 - 开头（防选项注入），只含常见 ref 字符。
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^-]*$`)

// ErrBadRef 表示 ref 不可解析（如 kb@TBD、含非法字符）。
var ErrBadRef = errors.New("knowledge: bad ref")

// NormalizeRef 把调用方或样本给出的 ref 规整为读取用的 ref：
// 空串 / WORKTREE → WORKTREE；kb@<sha> → <sha>；其余按 git ref 校验后原样返回。
func NormalizeRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == RefWorktree {
		return RefWorktree, nil
	}
	ref = strings.TrimPrefix(ref, sampleRefPrefix)
	if ref == "TBD" || !gitRefPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return "", fmt.Errorf("%w: %q", ErrBadRef, ref)
	}
	return ref, nil
}

// TreeReader 读取某个 ref 下 knowledge/ 目录的文件；path 一律相对 knowledge/，用 / 分隔。
type TreeReader interface {
	// List 返回 ref 下 knowledge/ 内全部文件路径（排序）。
	List(ctx context.Context, ref string) ([]string, error)
	// ReadFile 读取 ref 下 knowledge/<path>。
	ReadFile(ctx context.Context, ref, path string) ([]byte, error)
}

// RefResolver 是 TreeReader 的可选扩展：把可变 ref（分支名、HEAD）解析为不可变提交号，
// Store 据此做缓存键；未实现时按规整后的 ref 字符串缓存。
type RefResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// FSReader 从 fs.FS（根为 knowledge/ 目录）读取，忽略 ref；用于工作区与单测。
type FSReader struct {
	FS fs.FS
}

// List 实现 TreeReader。
func (r FSReader) List(_ context.Context, _ string) ([]string, error) {
	var out []string
	err := fs.WalkDir(r.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile 实现 TreeReader。
func (r FSReader) ReadFile(_ context.Context, _ string, p string) ([]byte, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	b, err := fs.ReadFile(r.FS, p)
	if err != nil {
		return nil, fmt.Errorf("knowledge: read %s: %w", p, err)
	}
	return b, nil
}

// GitReader 用 git 读取仓库 RepoDir 在某个提交上的 knowledge/：
// git ls-tree -r --name-only <ref> -- knowledge/ 与 git show <ref>:knowledge/<path>。
type GitReader struct {
	RepoDir string
}

// List 实现 TreeReader。
func (r GitReader) List(ctx context.Context, ref string) ([]string, error) {
	out, err := r.git(ctx, ref, "ls-tree", "-r", "--name-only", ref, "--", knowledgeDir+"/")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if rest, ok := strings.CutPrefix(line, knowledgeDir+"/"); ok && rest != "" {
			files = append(files, rest)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("knowledge: ref %s has no %s/ tree", ref, knowledgeDir)
	}
	sort.Strings(files)
	return files, nil
}

// ReadFile 实现 TreeReader。
func (r GitReader) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	if err := checkPath(p); err != nil {
		return nil, err
	}
	return r.git(ctx, ref, "show", ref+":"+knowledgeDir+"/"+p)
}

// Resolve 把 ref 解析为完整提交号，供 Store 按不可变的提交号缓存。
func (r GitReader) Resolve(ctx context.Context, ref string) (string, error) {
	out, err := r.git(ctx, ref, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// git 执行 git 子命令；ref 先校验，防止以 - 开头被当作选项。
func (r GitReader) git(ctx context.Context, ref string, args ...string) ([]byte, error) {
	if !gitRefPattern.MatchString(ref) {
		return nil, fmt.Errorf("%w: %q", ErrBadRef, ref)
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.RepoDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("knowledge: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// RepoReader 是生产实现：WORKTREE 读 RepoDir/knowledge 目录，其余 ref 经 git 读取。
type RepoReader struct {
	worktree FSReader
	git      GitReader
}

// NewRepoReader 以仓库根目录构造读取器。
func NewRepoReader(repoDir string) RepoReader {
	return RepoReader{
		worktree: FSReader{FS: os.DirFS(filepath.Join(repoDir, knowledgeDir))},
		git:      GitReader{RepoDir: repoDir},
	}
}

func (r RepoReader) pick(ref string) TreeReader {
	if ref == RefWorktree {
		return r.worktree
	}
	return r.git
}

// List 实现 TreeReader。
func (r RepoReader) List(ctx context.Context, ref string) ([]string, error) {
	return r.pick(ref).List(ctx, ref)
}

// ReadFile 实现 TreeReader。
func (r RepoReader) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	return r.pick(ref).ReadFile(ctx, ref, p)
}

// Resolve 实现 RefResolver：WORKTREE 原样返回，其余解析为提交号。
func (r RepoReader) Resolve(ctx context.Context, ref string) (string, error) {
	if ref == RefWorktree {
		return RefWorktree, nil
	}
	return r.git.Resolve(ctx, ref)
}

// checkPath 拒绝绝对路径与 .. 越界（路径来自 index.yaml，属于不完全可信输入）。
func checkPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || p == ".." {
		return fmt.Errorf("knowledge: bad path %q", p)
	}
	return nil
}
