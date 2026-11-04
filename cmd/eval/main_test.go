package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/agent/tools/mcp"
)

const repoRoot = "../.."

// TestNewReceiptSecret（P1-L4 合并统一）：评测回执密钥进程内由 crypto/rand 生成，32 字节且每次不同（不读 deploy/.env）。
func TestNewReceiptSecret(t *testing.T) {
	a, err := newReceiptSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newReceiptSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != receiptSecretLen || len(b) != receiptSecretLen || bytes.Equal(a, b) {
		t.Fatalf("secret lengths %d/%d, equal=%t", len(a), len(b), bytes.Equal(a, b))
	}
}

// P1 L6：评测 run 的 FixtureMCP 用 configs/tools.yaml 合并后的回落时间窗（与生产网关同一合并），validate 的
// fixtures.mcp.*.cache 校验与无库评测用 mcp.DefaultCacheRule；两者逐 server 必须一致（应失败：改了 tools.yaml 的
// max_staleness 而样本仍按旧边界写，评测与生产回落判定分叉）。
func TestLoadCacheRule_MatchesSampleDefaults(t *testing.T) {
	got, err := loadCacheRule(filepath.Join(repoRoot, defaultToolsFile))
	if err != nil {
		t.Fatal(err)
	}
	def := mcp.DefaultCacheRule()
	for _, server := range []string{"weather", "crop", "field", "device"} {
		gf, gm := got.Window(server)
		df, dm := def.Window(server)
		if gf != df || gm != dm {
			t.Errorf("server %s: tools.yaml window %v/%v, sample defaults %v/%v", server, gf, gm, df, dm)
		}
	}
}

func TestRun_ExitCodes(t *testing.T) {
	// 指向不可达端口：run 必须明确报错退出，不得静默跳过 DB
	t.Setenv("AS_MYSQL_PORT", "1")
	for _, k := range []string{"AS_MYSQL_PASSWORD", "AS_REDIS_PASSWORD", "AS_MINIO_ACCESS_KEY", "AS_MINIO_SECRET_KEY"} {
		t.Setenv(k, "unit-test-dummy")
	}
	badDir := filepath.Join(t.TempDir(), "regression")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	noAuthor := `id: tmp-no-author
version: 1
suite: regression
tags: [L4, protect.timing]
question: 要不要打药？
context: {farm: farm-demo-01, field: field-001, role: farm_manager, user: u-1}
fixtures: {clock: "2026-07-15T09:00:00+08:00"}
knowledge_ref: WORKTREE
expect: {outcome: REFUSED, refuse_reason: KB_MISS}
review_status: pending_expert
signed_at: 2026-10-28
`
	if err := os.WriteFile(filepath.Join(badDir, "tmp-no-author.yaml"), []byte(noAuthor), 0o644); err != nil {
		t.Fatal(err)
	}
	// 参考解自相矛盾：must_call 与 must_not_call 冲突 → reference_passes_graders 违规
	refDir := filepath.Join(t.TempDir(), "regression")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contra := strings.Replace(noAuthor, "tmp-no-author", "tmp-contra", 1) + "author: tester\n"
	contra = strings.Replace(contra, "expect: {outcome: REFUSED, refuse_reason: KB_MISS}",
		"expect: {outcome: REFUSED, refuse_reason: KB_MISS, trajectory: {must_call: [device.list], must_not_call: [device.*]}}", 1)
	if err := os.WriteFile(filepath.Join(refDir, "tmp-contra.yaml"), []byte(contra), 0o644); err != nil {
		t.Fatal(err)
	}
	tax := filepath.Join(repoRoot, defaultTax)
	suites := filepath.Join(repoRoot, defaultDir)
	outDir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args", args: nil, wantCode: exitUsage},
		{name: "unknown subcommand", args: []string{"foo"}, wantCode: exitUsage},
		// 535 个样本全部静态校验（含 P1 L5 审批样本 rice-approval-0001..0011、P1 L6 故障样本 rice-l6fault-0001..0007、
		// P1 L8 redteam 五类 39 条与演练沉淀 rice-dispatch-0004..0005、P2 L1-T capability 冒烟 rice-cap-0001..0002、
		// P2 L1 四类 capability 样本 317 条）；其中 rice-rcpt-0001..0006 共 6 个隔离（需 REAL 评测设施），run 只跑 529 个。
		{name: "validate repo samples", wantCode: exitOK, wantStdout: "validate OK: 535 samples",
			args: []string{"validate", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot}},
		{name: "validate counts quarantined", wantCode: exitOK, wantStdout: "6 quarantined (run skips them)\n",
			args: []string{"validate", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot}},
		{name: "validate rejects missing author", wantCode: exitFailed, wantStderr: "failed sample ids: tmp-no-author",
			args: []string{"validate", "--suites-dir", filepath.Dir(badDir), "--taxonomy", tax, "--repo", repoRoot}},
		{name: "validate rejects failing reference solution", wantCode: exitFailed, wantStderr: "reference_passes_graders",
			args: []string{"validate", "--suites-dir", filepath.Dir(refDir), "--taxonomy", tax, "--repo", repoRoot}},
		{name: "run fails loudly when mysql down", wantCode: exitFailed, wantStderr: "eval infra unavailable",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir,
				"--config", filepath.Join(repoRoot, "configs/app.yaml"), "--env-file", filepath.Join(outDir, "none.env"),
				"--suite", "regression", "--n", "2", "--tags", "L5,irrigate.amount"}},
		{name: "run no matching tag", wantCode: exitFailed, wantStderr: "no sample matches",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir, "--tags", "nope"}},
		{name: "run n=0", wantCode: exitFailed, wantStderr: "n must be >= 1",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir, "--n", "0"}},
		{name: "run baseline needs n>=3", wantCode: exitFailed, wantStderr: "baseline needs n >= 3",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir, "--n", "2",
				"--baseline", "baseline_p0"}},
		{name: "run baseline bad name", wantCode: exitFailed, wantStderr: "baseline name",
			args: []string{"run", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--out", outDir,
				"--baseline", "../escape"}},
		{name: "kbdump regression", wantCode: exitOK, wantStdout: "kbdump: 137 samples",
			args: []string{"kbdump", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--suite", "regression"}},
		{name: "kbdump unknown suite", wantCode: exitFailed, wantStderr: "no sample matches",
			args: []string{"kbdump", "--suites-dir", suites, "--taxonomy", tax, "--repo", repoRoot, "--suite", "nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d; stdout=%s stderr=%s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want containing %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
