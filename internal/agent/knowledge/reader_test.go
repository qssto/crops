package knowledge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// gitRepo 在临时目录建一个 git 仓，写入 files（相对仓库根）并提交；core.quotePath 保持 git 默认（true）。
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	runGitT(t, dir, "init", "-q")
	runGitT(t, dir, "config", "core.quotePath", "true") // 显式用 git 默认值，不受本机全局配置影响
	for p, c := range files {
		writeFileT(t, filepath.Join(dir, p), c)
	}
	runGitT(t, dir, "add", "-A")
	runGitT(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	return dir
}

func runGitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGitReader_List_NonASCII：git 默认 quotePath 会给非 ASCII 文件名加引号与八进制转义，List 须用 -z 原样列出。
func TestGitReader_List_NonASCII(t *testing.T) {
	dir := gitRepo(t, map[string]string{"knowledge/index.yaml": "x: 1\n", "knowledge/水稻/稻瘟病.md": "A", "README.md": "r"})
	got, err := GitReader{RepoDir: dir}.List(t.Context(), "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"index.yaml", "水稻/稻瘟病.md"}; !slices.Equal(got, want) {
		t.Fatalf("List = %q, want %q", got, want)
	}
	b, err := GitReader{RepoDir: dir}.ReadFile(t.Context(), "HEAD", "水稻/稻瘟病.md")
	if err != nil || string(b) != "A" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
}

// TestTrackedWorktreeReader：只列 git 已跟踪文件（含已暂存的新文件），内容取磁盘；未跟踪 / 被删的文件不列出；ref 只接受 WORKTREE。
func TestTrackedWorktreeReader(t *testing.T) {
	dir := gitRepo(t, map[string]string{"knowledge/index.yaml": "x: 1\n", "knowledge/水稻/稻瘟病.md": "A", "knowledge/sub/b.md": "B"})
	writeFileT(t, filepath.Join(dir, "knowledge/水稻/稻瘟病.md.bak"), "old")
	writeFileT(t, filepath.Join(dir, "knowledge/staged.md"), "S")
	runGitT(t, dir, "add", "knowledge/staged.md")
	writeFileT(t, filepath.Join(dir, "knowledge/sub/b.md"), "B2")
	if err := os.Remove(filepath.Join(dir, "knowledge/index.yaml")); err != nil {
		t.Fatal(err)
	}
	r := TrackedWorktreeReader{RepoDir: dir}
	got, err := r.List(t.Context(), RefWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"staged.md", "sub/b.md", "水稻/稻瘟病.md"}; !slices.Equal(got, want) {
		t.Fatalf("List = %q, want %q", got, want)
	}
	if b, err := r.ReadFile(t.Context(), RefWorktree, "sub/b.md"); err != nil || string(b) != "B2" {
		t.Fatalf("ReadFile = %q, %v (must read the worktree)", b, err)
	}
	if _, err := r.List(t.Context(), "HEAD"); !errors.Is(err, ErrBadRef) {
		t.Fatalf("non-WORKTREE ref: err = %v", err)
	}
}

// TestReaders_RejectSymlink：已跟踪的符号链接两种读法都报错；工作区里把已跟踪文件或目录换成符号链接也报错。
func TestReaders_RejectSymlink(t *testing.T) {
	dir := gitRepo(t, map[string]string{"knowledge/index.yaml": "x: 1\n", "knowledge/rice/a.md": "A"})
	if err := os.Symlink("rice/a.md", filepath.Join(dir, "knowledge/link.md")); err != nil {
		t.Fatal(err)
	}
	runGitT(t, dir, "add", "knowledge/link.md")
	runGitT(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "link")
	if _, err := (GitReader{RepoDir: dir}).List(t.Context(), "HEAD"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("git tracked symlink: err = %v, want ErrSymlink", err)
	}
	if _, err := (TrackedWorktreeReader{RepoDir: dir}).List(t.Context(), RefWorktree); !errors.Is(err, ErrSymlink) {
		t.Fatalf("worktree tracked symlink: err = %v, want ErrSymlink", err)
	}

	// 已跟踪的是普通文件，工作区里把所在目录换成符号链接（未提交）。
	dir2 := gitRepo(t, map[string]string{"knowledge/index.yaml": "x: 1\n", "knowledge/rice/a.md": "A"})
	writeFileT(t, filepath.Join(dir2, "elsewhere/a.md"), "evil")
	if err := os.RemoveAll(filepath.Join(dir2, "knowledge/rice")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir2, "elsewhere"), filepath.Join(dir2, "knowledge/rice")); err != nil {
		t.Fatal(err)
	}
	r := TrackedWorktreeReader{RepoDir: dir2}
	if _, err := r.List(t.Context(), RefWorktree); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlinked dir: List err = %v, want ErrSymlink", err)
	}
	if _, err := r.ReadFile(t.Context(), RefWorktree, "rice/a.md"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("symlinked dir: ReadFile err = %v, want ErrSymlink", err)
	}
}
