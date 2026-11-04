package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// holdoutIDMarker 是 holdout 样本 id 前缀（私有仓 rice-ho-<分类>-NNNN）；公开历史中出现即视为泄露。
const holdoutIDMarker = "rice-" + "ho-"

var errNotRepo = errors.New("not a git repository")

// historyGit 在 dir 执行 git 返回 stdout。
func historyGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return nil, errNotRepo
		}
		return nil, fmt.Errorf("git %v: %w: %s", args, err, stderr.String())
	}
	return out, nil
}

// errShallow 表示浅克隆：历史不全，不能断言「公开历史干净」。
var errShallow = errors.New("shallow clone: history incomplete")

// leakScan 是 holdoutLeak 的结论：Leak 非空为泄露（只报提交号，不带命中行，避免把 holdout 内容带进 CI 日志）；
// Mounted 表示 HEAD 的 eval/suites/holdout 是 submodule 指针（mode 160000）。
type leakScan struct {
	Leak    string
	Mounted bool
}

// holdoutLeak 检查 dir 仓库全部 ref 的 eval/suites/ 历史（文件名与内容）里有无 holdout 样本 ID，
// 以及 HEAD 的 eval/suites/holdout 是否只是 submodule 指针。L2 评审 P2-5：
//   - --diff-merges=separate：merge 提交对每个父提交各出一份 diff，merge 里新引入的文件与内容也查；
//   - --text：二进制文件按文本出 diff，不只是「Binary files differ」；
//   - 浅克隆返回 errShallow（历史不全，不能判干净）。
func holdoutLeak(dir string) (leakScan, error) {
	shallow, err := historyGit(dir, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return leakScan{}, err
	}
	if strings.TrimSpace(string(shallow)) == "true" {
		return leakScan{}, errShallow
	}
	names, err := historyGit(dir, "log", "--all", "--diff-merges=separate", "--format=", "--name-only", "--", "eval/suites/")
	if err != nil {
		return leakScan{}, err
	}
	if bytes.Contains(names, []byte(holdoutIDMarker)) {
		return leakScan{Leak: "holdout sample file name in eval/suites/ history"}, nil
	}
	patch, err := historyGit(dir, "log", "--all", "--diff-merges=separate", "--text", "--format=commit:%H", "-p", "--", "eval/suites/")
	if err != nil {
		return leakScan{}, err
	}
	commit := ""
	for _, line := range strings.Split(string(patch), "\n") {
		if c, ok := strings.CutPrefix(line, "commit:"); ok {
			commit = c
			continue
		}
		if strings.Contains(line, holdoutIDMarker) {
			return leakScan{Leak: "holdout sample id in eval/suites/ history, commit " + commit}, nil
		}
	}
	tree, err := historyGit(dir, "ls-tree", "-r", "HEAD", "--", "eval/suites/holdout")
	if err != nil {
		return leakScan{}, err
	}
	var scan leakScan
	for _, line := range strings.Split(strings.TrimSpace(string(tree)), "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "160000 commit "):
			scan.Mounted = true
		default:
			return leakScan{Leak: "eval/suites/holdout is not a submodule pointer"}, nil
		}
	}
	return scan, nil
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
}

// 验收 4 后半：本仓库所有 ref 的 eval/suites/ 公开历史中没有 holdout 样本 ID，且 HEAD 挂的是 submodule 指针。
// 浅克隆或当前分支未挂 submodule 时 Skip 并写明原因（不报 PASS）。
func TestHoldout_PublicHistoryClean(t *testing.T) {
	requireGit(t)
	scan, err := holdoutLeak(repoRoot)
	if errors.Is(err, errNotRepo) || errors.Is(err, errShallow) {
		t.Skipf("public history not verified: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if scan.Leak != "" {
		t.Fatal(scan.Leak)
	}
	if !scan.Mounted {
		t.Skip("history has no holdout id, but HEAD has no eval/suites/holdout submodule pointer (160000) on this branch: mount not verified")
	}
}

// 检查本身应失败的用例：临时仓库里提交过又删掉的 holdout 样本、holdout 目录下的普通文件都要被发现。
func TestHoldoutLeak_Detects(t *testing.T) {
	requireGit(t)
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{name: "clean", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/regression/a.yaml", "id: rice-pub-0001\n", "pub")
		}},
		{name: "content removed later", want: "commit ", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/regression/a.yaml", "id: "+holdoutIDMarker+"prot-0001\n", "leak")
			commitFile(t, dir, "eval/suites/regression/a.yaml", "id: rice-pub-0001\n", "fix")
		}},
		{name: "file name", want: "file name", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/regression/"+holdoutIDMarker+"x.yaml", "id: x\n", "leak")
		}},
		{name: "plain file under holdout", want: "submodule pointer", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/holdout/protect/x.yaml", "id: x\n", "leak")
		}},
		// L2 评审 P2-5：只在 merge 提交里引入的内容（evil merge）、二进制文件里的 id 都要发现。
		{name: "content only in merge", want: "commit ", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/regression/a.yaml", "id: rice-pub-0001\n", "base")
			mustGit(t, dir, "checkout", "-q", "-b", "side")
			commitFile(t, dir, "eval/suites/regression/b.yaml", "id: rice-pub-0002\n", "side")
			mustGit(t, dir, "checkout", "-q", "-")
			commitFile(t, dir, "eval/suites/regression/c.yaml", "id: rice-pub-0003\n", "main")
			mustGit(t, dir, "merge", "-q", "--no-ff", "--no-commit", "side")
			writeRepoFile(t, dir, "eval/suites/regression/m.yaml", "id: "+holdoutIDMarker+"prot-0002\n")
			mustGit(t, dir, "add", "-A")
			mustGit(t, dir, "commit", "-q", "-m", "merge")
		}},
		{name: "binary file", want: "commit ", setup: func(t *testing.T, dir string) {
			commitFile(t, dir, "eval/suites/regression/a.bin", "\x00\x01id: "+holdoutIDMarker+"prot-0003\x00", "bin")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mustGit(t, dir, "init", "-q")
			tt.setup(t, dir)
			scan, err := holdoutLeak(dir)
			if err != nil {
				t.Fatal(err)
			}
			if (tt.want == "") != (scan.Leak == "") || !strings.Contains(scan.Leak, tt.want) {
				t.Fatalf("leak = %q, want containing %q", scan.Leak, tt.want)
			}
		})
	}
}

// 没有 submodule 指针时不算「已挂载」；有指针时算；浅克隆返回 errShallow（测试据此 Skip，不报 PASS）。
func TestHoldoutLeak_MountAndShallow(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	commitFile(t, dir, "eval/suites/regression/a.yaml", "id: rice-pub-0001\n", "pub")
	commitFile(t, dir, "eval/suites/regression/b.yaml", "id: rice-pub-0002\n", "pub2")
	if scan, err := holdoutLeak(dir); err != nil || scan.Leak != "" || scan.Mounted {
		t.Fatalf("no pointer: scan = %+v, %v", scan, err)
	}
	head, err := historyGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(string(head))+",eval/suites/holdout")
	mustGit(t, dir, "commit", "-q", "-m", "mount")
	if scan, err := holdoutLeak(dir); err != nil || scan.Leak != "" || !scan.Mounted {
		t.Fatalf("pointer: scan = %+v, %v", scan, err)
	}
	clone := filepath.Join(t.TempDir(), "shallow")
	mustGit(t, dir, "clone", "-q", "--depth", "1", "file://"+dir, clone)
	if _, err := holdoutLeak(clone); !errors.Is(err, errShallow) {
		t.Fatalf("shallow clone: err = %v, want errShallow", err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := historyGit(dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, rel, content, msg string) {
	t.Helper()
	writeRepoFile(t, dir, rel, content)
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", msg)
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
