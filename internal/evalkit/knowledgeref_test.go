package evalkit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

func kcOf(t *testing.T, files map[string]string) string {
	t.Helper()
	m := fstest.MapFS{}
	for p, c := range files {
		m[p] = &fstest.MapFile{Data: []byte(c)}
	}
	h, err := KnowledgeContentRef(t.Context(), knowledge.FSReader{FS: m}, knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestKnowledgeContentRef：同内容同哈希；改内容、改路径、挪动路径与内容的边界都换哈希；隐藏文件不计入；空树报错。
func TestKnowledgeContentRef(t *testing.T) {
	base := map[string]string{"index.yaml": "x: 1\n", "rice/a.md": "A"}
	h := kcOf(t, base)
	if !strings.HasPrefix(h, KnowledgeContentPrefix) || len(h) != len(KnowledgeContentPrefix)+64 {
		t.Fatalf("hash = %q", h)
	}
	if again := kcOf(t, map[string]string{"rice/a.md": "A", "index.yaml": "x: 1\n"}); again != h {
		t.Fatalf("not deterministic: %s vs %s", again, h)
	}
	for name, files := range map[string]map[string]string{
		"content":  {"index.yaml": "x: 1\n", "rice/a.md": "B"},
		"path":     {"index.yaml": "x: 1\n", "rice/b.md": "A"},
		"added":    {"index.yaml": "x: 1\n", "rice/a.md": "A", "rice/c.md": ""},
		"boundary": {"index.yaml": "x: 1\n", "rice/a.mdA": ""},
	} {
		if kcOf(t, files) == h {
			t.Errorf("%s change must change the hash", name)
		}
	}
	if hidden := kcOf(t, map[string]string{"index.yaml": "x: 1\n", "rice/a.md": "A", ".DS_Store": "junk", "rice/.x/y": "z"}); hidden != h {
		t.Fatalf("hidden files must not count: %s vs %s", hidden, h)
	}
	empty := knowledge.FSReader{FS: fstest.MapFS{".DS_Store": &fstest.MapFile{Data: []byte("x")}}}
	if _, err := KnowledgeContentRef(t.Context(), empty, knowledge.RefWorktree); err == nil {
		t.Fatal("empty knowledge tree must fail")
	}
}

// refReader 按 ref 返回不同的知识树（模拟工作区与各提交）。
type refReader map[string]fstest.MapFS

func (r refReader) tree(ref string) (knowledge.FSReader, error) {
	m, ok := r[ref]
	if !ok {
		return knowledge.FSReader{}, errors.New("unknown ref " + ref)
	}
	return knowledge.FSReader{FS: m}, nil
}

func (r refReader) List(ctx context.Context, ref string) ([]string, error) {
	t, err := r.tree(ref)
	if err != nil {
		return nil, err
	}
	return t.List(ctx, ref)
}

func (r refReader) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	t, err := r.tree(ref)
	if err != nil {
		return nil, err
	}
	return t.ReadFile(ctx, ref, p)
}

// TestKnowledgeRef：WORKTREE 与空串读 worktreeRef（运行时 WORKTREE、补录时 git_sha）；同内容的不同 ref 去重；
// 多份内容排序逗号拼接；坏 ref、无样本报错。
func TestKnowledgeRef(t *testing.T) {
	treeA := fstest.MapFS{"a.md": &fstest.MapFile{Data: []byte("A")}}
	treeB := fstest.MapFS{"a.md": &fstest.MapFile{Data: []byte("B")}}
	r := refReader{knowledge.RefWorktree: treeA, "c261f71": treeB, "abc1234": treeA}
	ctx := t.Context()
	hA, err := KnowledgeContentRef(ctx, r, knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	hB, err := KnowledgeContentRef(ctx, r, "c261f71")
	if err != nil {
		t.Fatal(err)
	}
	ws := []*sample.Sample{{ID: "s1", KnowledgeRef: "WORKTREE"}, {ID: "s2", KnowledgeRef: ""}}
	if got, err := KnowledgeRef(ctx, r, ws, knowledge.RefWorktree); err != nil || got != hA {
		t.Fatalf("worktree = %q %v, want %q", got, err, hA)
	}
	if got, err := KnowledgeRef(ctx, r, ws, "c261f71"); err != nil || got != hB {
		t.Fatalf("backfill at git_sha = %q %v, want %q", got, err, hB)
	}
	mixed := append(ws, &sample.Sample{ID: "s3", KnowledgeRef: "kb@abc1234"}, &sample.Sample{ID: "s4", KnowledgeRef: "kb@c261f71"})
	want := []string{hA, hB}
	if hB < hA {
		want = []string{hB, hA}
	}
	if got, err := KnowledgeRef(ctx, r, mixed, knowledge.RefWorktree); err != nil || got != strings.Join(want, ",") {
		t.Fatalf("mixed = %q %v, want %q", got, err, strings.Join(want, ","))
	}
	if _, err := KnowledgeRef(ctx, r, []*sample.Sample{{ID: "bad", KnowledgeRef: "kb@TBD"}}, knowledge.RefWorktree); !errors.Is(err, knowledge.ErrBadRef) {
		t.Fatalf("bad ref: err = %v", err)
	}
	if _, err := KnowledgeRef(ctx, r, nil, knowledge.RefWorktree); err == nil {
		t.Fatal("no samples must fail")
	}
}

// TestJudgeVersionsKey_NoCollision：维度或版本里含 , / = 时不同映射不能拼出同一文本（旧实现 "a=b,c=d" 撞车）。
func TestJudgeVersionsKey_NoCollision(t *testing.T) {
	a := Snapshot{JudgeVersions: map[string]string{"a": "b,c=d"}}
	b := Snapshot{JudgeVersions: map[string]string{"a": "b", "c": "d"}}
	if a.JudgeVersionsKey() == b.JudgeVersionsKey() {
		t.Fatalf("collision: %q", a.JudgeVersionsKey())
	}
	c := Snapshot{JudgeVersions: map[string]string{"a=b": "c"}}
	d := Snapshot{JudgeVersions: map[string]string{"a": "b=c"}}
	if c.JudgeVersionsKey() == d.JudgeVersionsKey() {
		t.Fatalf("collision: %q", c.JudgeVersionsKey())
	}
	if (Snapshot{}).JudgeVersionsKey() != "" || (Snapshot{JudgeVersions: map[string]string{}}).JudgeVersionsKey() != "" {
		t.Fatal("no judge must be empty")
	}
	x := Snapshot{JudgeVersions: map[string]string{"fact": "j1", "cite": "j2"}}
	if got := x.JudgeVersionsKey(); got != `"cite"="j2","fact"="j1"` {
		t.Fatalf("key = %s", got)
	}
}

// git 在 dir 执行（隔离全局 / 系统配置，core.quotePath 用默认值 true）。
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func putT(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestKnowledgeContentRef_WorktreeEqualsHEAD：真实临时 git 仓，同一内容 WORKTREE 与 HEAD 算出同一哈希；
// 覆盖中文文件名、子目录、未跟踪文件（备份 / 新文件不计入）、工作区未提交改动（如实反映）。
func TestKnowledgeContentRef_WorktreeEqualsHEAD(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	for rel, c := range map[string]string{"knowledge/index.yaml": "x: 1\n", "knowledge/水稻/稻瘟病.md": "A",
		"knowledge/rice/sub/b.md": "B", "knowledge/.gitkeep": "", "README.md": "r"} {
		putT(t, dir, rel, c)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	putT(t, dir, "knowledge/水稻/稻瘟病.md.bak", "old")
	putT(t, dir, "knowledge/rice/untracked.md", "U")
	putT(t, dir, "knowledge/新目录/草稿.md", "D")
	r := knowledge.NewTrackedRepoReader(dir)
	ctx := t.Context()
	head, err := KnowledgeContentRef(ctx, r, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	wt, err := KnowledgeContentRef(ctx, r, knowledge.RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if wt != head {
		t.Fatalf("WORKTREE %s != HEAD %s (untracked files must not count)", wt, head)
	}
	// 中文文件名确实计入：去掉它哈希要变。
	want := kcOf(t, map[string]string{"index.yaml": "x: 1\n", "水稻/稻瘟病.md": "A", "rice/sub/b.md": "B"})
	if head != want {
		t.Fatalf("HEAD %s != in-memory tree %s (non-ASCII path dropped?)", head, want)
	}
	putT(t, dir, "knowledge/水稻/稻瘟病.md", "A2")
	if dirty, err := KnowledgeContentRef(ctx, r, knowledge.RefWorktree); err != nil || dirty == head {
		t.Fatalf("uncommitted change must change WORKTREE hash: %s %v", dirty, err)
	}
}

// TestKnowledgeContentRef_SymlinkRejected：knowledge/ 下有符号链接时 WORKTREE 与 git 两种读法都报错。
func TestKnowledgeContentRef_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	putT(t, dir, "knowledge/index.yaml", "x: 1\n")
	if err := os.Symlink("index.yaml", filepath.Join(dir, "knowledge", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	r := knowledge.NewTrackedRepoReader(dir)
	for _, ref := range []string{knowledge.RefWorktree, "HEAD"} {
		if _, err := KnowledgeContentRef(t.Context(), r, ref); !errors.Is(err, knowledge.ErrSymlink) {
			t.Fatalf("%s: err = %v, want ErrSymlink", ref, err)
		}
	}
}
