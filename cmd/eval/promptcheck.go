package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// errPromptVersion 表示 prompts/ 相对 base 有改动而 configs/model.yaml 的 prompt_version 没变（P2 §6.8 L7-a 任务 8）：
// 快照与 compare 以 prompt_version 为可比因子，提示词改了版本不变会让对比把两份不同的提示词当成同一因子值。
var errPromptVersion = errors.New("prompts/ changed but model.prompt_version did not")

// cmdPromptCheck 是 CI 的 prompt_version 检查：eval promptcheck --base origin/main。
func cmdPromptCheck(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("promptcheck", flag.ContinueOnError)
	base := fs.String("base", "", "对比基准 git ref（如 origin/main；CI 取 PR 的 base sha）")
	repo := fs.String("repo", defaultRepo, "仓库根目录")
	modelFile := fs.String("model-config", "configs/model.yaml", "模型配置（相对 --repo）")
	prompts := fs.String("prompts", "prompts", "提示词目录（相对 --repo）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" {
		return errors.New("promptcheck needs --base <git ref>")
	}
	return promptCheck(ctx, *repo, *base, *modelFile, *prompts, stdout)
}

// promptCheck 比较 merge-base(base, HEAD) 与工作区（含已提交与未提交改动）：prompts 目录有改动时要求 prompt_version
// 与 merge-base 不同。用 merge-base 而非 base 本身：base 分叉后自己的提示词 / 版本变化不算本分支的改动。
func promptCheck(ctx context.Context, repo, base, modelFile, prompts string, stdout io.Writer) error {
	mb, err := gitIn(ctx, repo, "merge-base", base, "HEAD")
	if err != nil {
		return err
	}
	fork := strings.TrimSpace(mb)
	out, err := gitIn(ctx, repo, "diff", "--name-only", fork, "--", prompts)
	if err != nil {
		return err
	}
	changed := strings.Fields(out)
	if len(changed) == 0 {
		fmt.Fprintf(stdout, "promptcheck OK: %s unchanged since merge-base(%s, HEAD) %s\n", prompts, base, fork)
		return nil
	}
	baseRaw, err := gitIn(ctx, repo, "show", fork+":"+filepath.ToSlash(modelFile))
	if err != nil {
		return err
	}
	baseVer, err := promptVersionOf([]byte(baseRaw))
	if err != nil {
		return fmt.Errorf("%s@%s: %w", modelFile, fork, err)
	}
	headRaw, err := os.ReadFile(filepath.Join(repo, modelFile))
	if err != nil {
		return fmt.Errorf("read %s: %w", modelFile, err)
	}
	headVer, err := promptVersionOf(headRaw)
	if err != nil {
		return fmt.Errorf("%s: %w", modelFile, err)
	}
	if baseVer == headVer {
		return fmt.Errorf("%w: prompt_version=%s, changed: %s", errPromptVersion, headVer, strings.Join(changed, " "))
	}
	fmt.Fprintf(stdout, "promptcheck OK: %d files under %s changed, prompt_version %s -> %s\n", len(changed), prompts, baseVer, headVer)
	return nil
}

// promptVersionOf 取模型配置的 model.prompt_version（只读这一项，不走 Viper：环境变量覆盖不算版本变更）。
func promptVersionOf(raw []byte) (string, error) {
	var doc struct {
		Model struct {
			PromptVersion string `yaml:"prompt_version"`
		} `yaml:"model"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if doc.Model.PromptVersion == "" {
		return "", errors.New("model.prompt_version is empty")
	}
	return doc.Model.PromptVersion, nil
}

// gitIn 在 repo 下执行 git 并返回 stdout；出错时带上 stderr。
func gitIn(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
