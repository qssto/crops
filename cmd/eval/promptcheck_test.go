package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 改 prompts/ 未升 prompt_version → 失败；升了 / 没改 prompts/ → 通过（应失败用例，临时 git 仓库）。
func TestPromptCheck(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	modelYAML := func(v string) string { return "model:\n  provider: stub\n  prompt_version: " + v + "\n" }
	git("init", "-q")
	write("configs/model.yaml", modelYAML("v1"))
	write("prompts/v1/role.md", "role\n")
	write("README", "x\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("tag", "base")
	check := func() error {
		var out strings.Builder
		return promptCheck(t.Context(), dir, "base", "configs/model.yaml", "prompts", &out)
	}
	write("README", "y\n")
	if err := check(); err != nil {
		t.Fatalf("no prompt change: %v", err)
	}
	write("prompts/v1/role.md", "role changed\n")
	if err := check(); !errors.Is(err, errPromptVersion) {
		t.Fatalf("prompt changed, version kept: %v", err)
	}
	git("commit", "-q", "-am", "prompt")
	if err := check(); !errors.Is(err, errPromptVersion) {
		t.Fatalf("committed prompt change, version kept: %v", err)
	}
	write("configs/model.yaml", modelYAML("v2"))
	if err := check(); err != nil {
		t.Fatalf("version bumped: %v", err)
	}
	if err := promptCheck(t.Context(), dir, "no-such-ref", "configs/model.yaml", "prompts", &strings.Builder{}); err == nil {
		t.Fatal("bad base ref passed")
	}

	// 与 merge-base 比较：base 分叉后自己升了 prompt_version，本分支改提示词却没升版本，仍须失败
	// （直接与 base 比会把 base 的版本变化当成本分支的升版本而放行）。
	git("commit", "-q", "-am", "bump v2")
	git("tag", "fork")
	git("checkout", "-q", "-b", "upstream")
	write("configs/model.yaml", modelYAML("v3"))
	write("prompts/v1/other.md", "upstream\n")
	git("add", ".")
	git("commit", "-q", "-m", "upstream bump")
	git("checkout", "-q", "fork")
	git("checkout", "-q", "-b", "feature")
	write("prompts/v1/role.md", "feature change\n")
	git("commit", "-q", "-am", "feature prompt")
	if err := promptCheck(t.Context(), dir, "upstream", "configs/model.yaml", "prompts", &strings.Builder{}); !errors.Is(err, errPromptVersion) {
		t.Fatalf("diverged base bumped version, branch kept it: %v", err)
	}
}
