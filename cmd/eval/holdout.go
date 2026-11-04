package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/sample"
)

const (
	// envEvalReader 是调用者身份（holdout 读者名单里的 id）；缺省视为无权限。
	envEvalReader = "AS_EVAL_READER"
	// defaultHoldoutReaders 是 --repo 下的 holdout 读者名单（只列负责人与 CI 服务账号）。
	defaultHoldoutReaders = "eval/holdout_readers.yaml"
)

// authorizeHoldout 按 AS_EVAL_READER 与读者名单签发 holdout 凭证，不在名单即失败关闭（不读任何 holdout 文件）。
func (f *sampleFlags) authorizeHoldout() error {
	path := filepath.Join(f.repo, defaultHoldoutReaders)
	g, err := sample.AuthorizeHoldout(path, os.Getenv(envEvalReader))
	if err != nil {
		return fmt.Errorf("holdout (set %s to a reader listed in %s): %w", envEvalReader, path, err)
	}
	f.holdout = g
	return nil
}

// loadOptions 是加载参数：holdout 按 --repo 下 eval/suites/holdout 的真实目录判定（--suites-dir 换成其子目录、
// 父目录或换大小写都绕不过），凭证只在 authorizeHoldout 通过后非 nil。
func (f *sampleFlags) loadOptions() sample.LoadOptions {
	return sample.LoadOptions{HoldoutDir: filepath.Join(f.repo, defaultDir, sample.HoldoutDir), Grant: f.holdout}
}

// wantsHoldout 报告 run 的套件是否显式包含 holdout（只有显式点名才读 holdout）。
func wantsHoldout(suites []string) bool { return slices.Contains(suites, sample.HoldoutDir) }

// writeReport 是 run 与 resume 共用的出报告步骤：holdout 明细先上传 report/holdout/ 前缀（有 holdout trial 时打印对象键），
// 本地报告只写脱敏副本；返回脱敏后的实验与本地报告路径。
func writeReport(ctx context.Context, obj report.ObjectPutter, bucket, outRoot string, exp *evalkit.Experiment,
	stdout io.Writer) (*evalkit.Experiment, string, error) {
	hoKey, err := report.UploadHoldoutReport(ctx, obj, bucket, exp)
	if err != nil {
		return nil, "", err
	}
	if hoKey != "" {
		fmt.Fprintf(stdout, "holdout report: %s/%s\n", bucket, hoKey)
	}
	pub := report.RedactHoldout(exp)
	path, err := report.WriteFile(outRoot, pub)
	if err != nil {
		return nil, "", err
	}
	return pub, path, nil
}
