package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflow 是 GitHub Actions 工作流里本测试关心的字段（actionlint 不在仓库工具链内，用 yaml.v3 做结构校验）。
type workflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Jobs        map[string]struct {
		Uses  string            `yaml:"uses"`
		With  map[string]any    `yaml:"with"`
		Env   map[string]string `yaml:"env"`
		Steps []step            `yaml:"steps"`
	} `yaml:"jobs"`
}

type step struct {
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Run  string            `yaml:"run"`
	Uses string            `yaml:"uses"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func loadWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".github/workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(w.On) == 0 || len(w.Jobs) == 0 {
		t.Fatalf("%s: no on / jobs", name)
	}
	// 评审修复：每个 workflow 顶层只给最小权限 contents: read。
	if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
		t.Errorf("%s: permissions = %v, want {contents: read}", name, w.Permissions)
	}
	return w
}

// 三个入口都调 eval-gate.yml 并给出 mode 与 token_budget；PR 在改代码 / 样本 / 配置 / 提示词时触发，另有 go test ./... 作业。
func TestWorkflows_Entrypoints(t *testing.T) {
	cases := []struct {
		file, event, mode string
	}{
		{"pr-smoke.yml", "pull_request", modeSmoke},
		{"nightly.yml", "schedule", modeNightly},
		{"release.yml", "workflow_dispatch", modeRelease},
	}
	for _, c := range cases {
		w := loadWorkflow(t, c.file)
		if _, ok := w.On[c.event]; !ok {
			t.Errorf("%s: no %s trigger", c.file, c.event)
		}
		for name, j := range w.Jobs {
			if c.file == "pr-smoke.yml" && name == "unit" {
				continue
			}
			if j.Uses != "./.github/workflows/eval-gate.yml" || j.With["mode"] != c.mode || j.With["token_budget"] == nil {
				t.Errorf("%s/%s: uses=%q with=%v", c.file, name, j.Uses, j.With)
			}
		}
	}
	pr := loadWorkflow(t, "pr-smoke.yml")
	var on struct {
		Paths []string `yaml:"paths"`
	}
	node := pr.On["pull_request"]
	if err := node.Decode(&on); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"internal/**", "knowledge/**", "prompts/**", "cmd/**", "policy/**", "eval/**", "configs/**", "contracts/**"} {
		if !slices.Contains(on.Paths, p) {
			t.Errorf("pr-smoke paths %v lack %s", on.Paths, p)
		}
	}
	if j := pr.Jobs["smoke"]; j.With["prompt_base"] == nil {
		t.Error("pr-smoke does not run promptcheck")
	}
	if !slices.ContainsFunc(pr.Jobs["unit"].Steps, func(s step) bool { return strings.TrimSpace(s.Run) == "go test ./..." }) {
		t.Error("pr-smoke has no go test ./... job")
	}
	if nb := loadWorkflow(t, "nightly.yml").Jobs["nightly"].With["non_blocking"]; nb != true {
		t.Errorf("nightly non_blocking = %v", nb)
	}
}

// eval-gate：密钥只来自 secrets；有 promptcheck、--fail-on-unreleasable 自检与 compare、make eval MODE；run 脚本不直接插值 inputs。
func TestWorkflows_EvalGate(t *testing.T) {
	w := loadWorkflow(t, "eval-gate.yml")
	if _, ok := w.On["workflow_call"]; !ok {
		t.Fatal("eval-gate is not reusable")
	}
	j, ok := w.Jobs["eval"]
	if !ok {
		t.Fatal("no eval job")
	}
	for _, k := range []string{"MYSQL_ROOT_PASSWORD", "REDIS_PASSWORD", "MINIO_USER", "MINIO_PASSWORD", "AUTH_JWT_SECRET"} {
		if !strings.HasPrefix(j.Env[k], "${{ secrets.") {
			t.Errorf("env %s = %q, want a GitHub secret", k, j.Env[k])
		}
	}
	var runs []string
	for _, s := range j.Steps {
		if strings.Contains(s.Run, "${{") {
			t.Errorf("step %q interpolates expressions into the shell: %s", s.Name, s.Run)
		}
		runs = append(runs, s.Run)
	}
	all := strings.Join(runs, "\n")
	for _, want := range []string{"make eval-promptcheck", "TestL0_FailClosed_Integration", `make eval GO=go MODE="$EVAL_MODE" TOKEN_BUDGET=`,
		"FAIL_ON_UNRELEASABLE=1", "make migrate migrate-eval"} {
		if !strings.Contains(all, want) {
			t.Errorf("eval-gate steps lack %q", want)
		}
	}
	checkEvalGateSteps(t, j.Steps)
}

// checkEvalGateSteps 是评审修复项：checkout 不留凭据；迁移前检查迁移来源（缺失明确失败）；artifact 只传公开报告；
// 最后一步在失败时标红上报（nightly 非阻断也可见）。
func checkEvalGateSteps(t *testing.T, steps []step) {
	t.Helper()
	idx := func(f func(step) bool) int { return slices.IndexFunc(steps, f) }
	co := idx(func(s step) bool { return strings.HasPrefix(s.Uses, "actions/checkout@") })
	if co < 0 || steps[co].With["persist-credentials"] != false {
		t.Errorf("checkout must set persist-credentials: false (idx %d)", co)
	}
	mig := idx(func(s step) bool { return strings.Contains(s.Run, "make migrate migrate-eval") })
	src := idx(func(s step) bool {
		return strings.Contains(s.Run, "migrations/eval/*.up.sql") && strings.Contains(s.Run, "::error ") && strings.Contains(s.Run, "exit 1")
	})
	if src < 0 || src > mig {
		t.Errorf("no migration-source check before migrate (check %d, migrate %d)", src, mig)
	}
	up := idx(func(s step) bool { return strings.HasPrefix(s.Uses, "actions/upload-artifact@") })
	if up < 0 || steps[up].With["path"] != "eval/out/*/report.md" {
		t.Errorf("artifact must upload only public reports eval/out/*/report.md (idx %d)", up)
	}
	last := steps[len(steps)-1]
	if !strings.Contains(last.If, "failure()") || !strings.Contains(last.Run, "::error") || !strings.Contains(last.Run, "GITHUB_STEP_SUMMARY") {
		t.Errorf("last step must report failure visibly: if=%q run=%q", last.If, last.Run)
	}
}

// P2 L7-b：release 跑完对 gate_baseline（默认 baseline_p1）做质量门禁，非 PASS / 未签字的 NEED_SIGNOFF 退出非 0；
// 门禁只在 release 跑（PR smoke 不变），参数经环境变量进 make，门禁报告（holdout 只列 ID）另传 artifact。
func TestWorkflows_ReleaseGate(t *testing.T) {
	w := loadWorkflow(t, "eval-gate.yml")
	node := w.On["workflow_call"]
	var call struct {
		Inputs map[string]struct {
			Default any `yaml:"default"`
		} `yaml:"inputs"`
	}
	if err := node.Decode(&call); err != nil {
		t.Fatal(err)
	}
	// 评审 P2：gate_baseline 缺省为空（不隐式用 baseline_p1），空时 gate 读 eval/baselines/release.yaml 指针。
	if g, ok := call.Inputs["gate_baseline"]; !ok || g.Default != "" {
		t.Errorf("eval-gate input gate_baseline = %+v, want default empty (release pointer)", g)
	}
	j := w.Jobs["eval"]
	if j.Env["EVAL_GATE_BASELINE"] != "${{ inputs.gate_baseline }}" {
		t.Errorf("env EVAL_GATE_BASELINE = %q", j.Env["EVAL_GATE_BASELINE"])
	}
	run := slices.IndexFunc(j.Steps, func(s step) bool { return s.Name != "" && strings.Contains(s.Run, "make eval GO=go") })
	gi := slices.IndexFunc(j.Steps, func(s step) bool { return strings.Contains(s.Run, "make eval-gate") })
	if gi < 0 || gi < run {
		t.Fatalf("no make eval-gate step after make eval (gate %d, run %d)", gi, run)
	}
	g := j.Steps[gi]
	if g.If != "inputs.mode == 'release' && github.ref == 'refs/heads/main'" || g.Env["EVAL_CANDIDATE"] != "${{ steps.run.outputs.exp_id }}" ||
		!strings.Contains(g.Run, `make eval-gate GO=go GATE_BASELINE="$EVAL_GATE_BASELINE" EXP="$EVAL_CANDIDATE"`) {
		t.Errorf("gate step = %+v", g)
	}
	// 非 main 的 release：门禁步骤不跑时必须有一步明确失败（不能静默跳过）。
	if !slices.ContainsFunc(j.Steps, func(s step) bool {
		return s.If == "inputs.mode == 'release' && github.ref != 'refs/heads/main'" &&
			strings.Contains(s.Run, "::error") && strings.Contains(s.Run, "exit 1")
	}) {
		t.Error("release on a non-main ref must fail explicitly")
	}
	if !slices.ContainsFunc(j.Steps, func(s step) bool {
		return strings.HasPrefix(s.Uses, "actions/upload-artifact@") && s.With["path"] == "eval/out/gate/*.md"
	}) {
		t.Error("gate report is not uploaded")
	}
	rel := loadWorkflow(t, "release.yml").Jobs["release"]
	if rel.With["gate_baseline"] != "${{ inputs.gate_baseline }}" {
		t.Errorf("release with.gate_baseline = %v", rel.With["gate_baseline"])
	}
	for _, f := range []string{"pr-smoke.yml", "nightly.yml"} {
		for name, job := range loadWorkflow(t, f).Jobs {
			if _, ok := job.With["gate_baseline"]; ok {
				t.Errorf("%s/%s must not set gate_baseline (gate runs only in release)", f, name)
			}
		}
	}
}
