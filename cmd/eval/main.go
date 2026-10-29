// cmd/eval 是评测入口：
//
//	eval validate [--suites-dir eval/suites] [--taxonomy eval/taxonomy.yaml] [--repo .]
//	eval run --suite regression [--n 3] [--tags L2,protect] [--out eval/out]
//	eval kbdump --suite regression
//
// validate 只做样本静态校验、不装配 agent；run 先校验再经 app.Build 装配被测 agent 回放并出报告；
// kbdump 按样本情境（taxonomy tag + crop.phenology fixture）跑 progressive_loader，打印命中的 kb_id@version。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	defaultDir  = "eval/suites"
	defaultTax  = "eval/taxonomy.yaml"
	defaultOut  = "eval/out"
	defaultRepo = "."
	// contractsDir 是 --repo 下 L1 冻结契约目录（schema_valid 用）。
	contractsDir = "contracts"
	// toolPolicyFile 是 --repo 下工具策略（no_model_actuate 取其 actuate 集合，与生产同一份）。
	toolPolicyFile = "policy/tool_policy.yaml"
	usageString    = "usage: eval <validate|run|kbdump> [flags]"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usageString)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch args[0] {
	case "validate":
		err = cmdValidate(args[1:], stdout)
	case "run":
		err = cmdRun(ctx, args[1:], stdout, stderr)
	case "kbdump":
		err = cmdKBDump(ctx, args[1:], stdout)
	default:
		fmt.Fprintln(stderr, usageString)
		return exitUsage
	}
	if err == nil {
		return exitOK
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitUsage
	}
	fmt.Fprintln(stderr, "eval:", err)
	var verr *sample.ValidationError
	if errors.As(err, &verr) {
		fmt.Fprintln(stderr, "failed sample ids:", strings.Join(verr.Report.FailedIDs(), " "))
	}
	return exitFailed
}

// sampleFlags 是 validate / run / kbdump 共用的样本与知识库位置参数。
type sampleFlags struct {
	dir      string
	taxonomy string
	repo     string
}

func (f *sampleFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.dir, "suites-dir", defaultDir, "样本根目录（递归加载 **.yaml）")
	fs.StringVar(&f.taxonomy, "taxonomy", defaultTax, "question_taxonomy 文件")
	fs.StringVar(&f.repo, "repo", defaultRepo, "仓库根目录（其下 knowledge/ 为知识库；knowledge_ref 为 git ref 时在此仓库取）")
}

// loadedSamples 是通过静态校验的样本及校验所用的 taxonomy / 知识库 / 打分器；refNotes 为参考解不判定维度说明。
type loadedSamples struct {
	samples  []*sample.Sample
	tx       *knowledge.Taxonomy
	kb       *knowledge.Store
	graders  []grader.Grader
	refNotes []string
}

// loadValidSamples 加载并静态校验全部样本（含 L3 must_cite / product_in 知识库规则、L4 参考解过全部打分器）。
func (f *sampleFlags) loadValidSamples() (*loadedSamples, error) {
	tx, err := knowledge.LoadTaxonomy(f.taxonomy)
	if err != nil {
		return nil, err
	}
	kb := knowledge.NewStore(knowledge.NewRepoReader(f.repo))
	schema, err := code.NewSchemaValid(filepath.Join(f.repo, contractsDir))
	if err != nil {
		return nil, err
	}
	tp, err := policy.Load(filepath.Join(f.repo, toolPolicyFile))
	if err != nil {
		return nil, err
	}
	graders := code.All(schema, tp.Actuate())
	ref := code.NewReferenceCheck(tx, kb, graders)
	rules := append(sample.DefaultRules(tx, kb), ref)
	samples, err := sample.LoadAndValidate(f.dir, sample.NewValidator(rules...))
	if err != nil {
		return nil, err
	}
	return &loadedSamples{samples: samples, tx: tx, kb: kb, graders: graders, refNotes: ref.Notes()}, nil
}

func cmdValidate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ls, err := sf.loadValidSamples()
	if err != nil {
		return err
	}
	samples := ls.samples
	refused := 0
	for _, s := range samples {
		if s.IsRefusal() {
			refused++
		}
	}
	fmt.Fprintf(stdout, "validate OK: %d samples, %d refused (%.0f%%)\n",
		len(samples), refused, 100*float64(refused)/float64(len(samples)))
	if len(ls.refNotes) > 0 {
		fmt.Fprintf(stdout, "reference not judged: %s\n", strings.Join(ls.refNotes, "; "))
	}
	return nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	suites := fs.String("suite", "regression", "套件，逗号分隔")
	n := fs.Int("n", evalkit.DefaultTrials, "每个样本运行次数")
	tags := fs.String("tags", "", "只回放带任一 tag 的样本，逗号分隔，如 L2,protect")
	outRoot := fs.String("out", defaultOut, "报告输出根目录")
	var inf infraFlags
	inf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *n < 1 {
		return fmt.Errorf("n must be >= 1, got %d", *n)
	}

	ls, err := sf.loadValidSamples()
	if err != nil {
		return err
	}
	opts := evalkit.Options{Suites: splitList(*suites), Tags: splitList(*tags), N: *n}
	selected := evalkit.Select(ls.samples, opts.Suites, opts.Tags)
	if len(selected) == 0 {
		return fmt.Errorf("no sample matches suite=%v tags=%v", opts.Suites, opts.Tags)
	}

	in, err := inf.open(ctx, stderr, ls)
	if err != nil {
		return fmt.Errorf("eval infra unavailable (需先 docker compose up 并 make migrate migrate-eval): %w", err)
	}
	exp, err := runWithStore(ctx, in, ls, selected, opts)
	if err := errors.Join(err, in.close()); err != nil {
		return err
	}
	path, err := report.WriteFile(*outRoot, exp)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "eval done: exp_id=%s samples=%d trials=%d report=%s\n",
		exp.ID, exp.Samples, len(exp.Trials), path)
	return nil
}

// runWithStore 先在 agri_eval.experiment 建行，回放并跑全部打分器后同事务写 trial 并置 DONE。
func runWithStore(ctx context.Context, in *infra, ls *loadedSamples, samples []*sample.Sample, opts evalkit.Options) (*evalkit.Experiment, error) {
	wall := clock.System{}
	sha, err := gitSHA(ctx)
	if err != nil {
		return nil, err
	}
	exps := store.New(in.evalDB)
	id, err := exps.Start(ctx, opts.Suites, store.TriggerManual, sha, wall.Now())
	if err != nil {
		return nil, err
	}
	opts.ExperimentID = store.FormatID(id)
	runner, err := evalkit.NewRunner(evalkit.PoolEnv(in.pool, in.deps), wall.Now,
		evalkit.WithQuestion(in.question), evalkit.WithGraders(ls.kb, ls.graders...),
		evalkit.WithApprover(evalkit.ScriptApprover(scriptApprovers())))
	if err != nil {
		return nil, err
	}
	exp, err := runner.Run(ctx, samples, opts)
	if err != nil {
		return nil, err
	}
	if err := exps.Finish(ctx, id, exp); err != nil {
		return nil, err
	}
	return exp, nil
}

func gitSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
