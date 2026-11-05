// cmd/eval 是评测入口：
//
//	eval validate [--suites-dir eval/suites] [--taxonomy eval/taxonomy.yaml] [--repo .] [--coverage-targets ...] [--strict-coverage]
//	eval run --suite regression [--n 3] [--tags L2,protect] [--out eval/out] [--baseline baseline_p0 [--baselines-dir eval/baselines]] [--flags rule_offload.tank_mix=on]
//	eval kbdump --suite regression
//	eval graduate [--apply]
//	eval baseline freeze --name baseline_p1 --exp 102 [--backfill eval/baselines/baseline_p1_exit.yaml]
//	eval compare --baseline <名|exp id> --candidate <id> [--mode factor|phase] [--reference <id>] [--fail-on-unreleasable]
//	eval aa [--runs 3 | --from-exps a,b,c] [--out eval/gates/thresholds.yaml]
//	eval resume --exp <id> [--holdout]（P2 L7-a：补跑缺失 / INFRA_ERROR trial，见 resume.go）
//	eval promptcheck --base origin/main（P2 L7-a：prompts/ 改了 prompt_version 必须变，见 promptcheck.go）
//
// validate 只做样本静态校验、不装配 agent，并输出分层覆盖报表与补写清单（P2 L1-T）；run 先校验再经 app.Build 装配被测 agent 回放并出报告
// （--baseline 时另把报告与配置快照上传 agri-steward-eval 的 report/{exp_id}/ 并写 baseline yaml）；
// kbdump 按样本情境（taxonomy tag + crop.phenology fixture）跑 progressive_loader，打印命中的 kb_id@version；
// graduate 按路线 §6.2 列出可从 capability 毕业的样本，--apply 时移到 regression/（P2 L1-T）。
// baseline freeze / compare / aa 是 P2 L0：冻结基线（P1 实验补录快照）、单因子 / 阶段级对比、A/A 噪声阈值草稿。
// 三者失败关闭：实验须 DONE 且有 trial、快照完整，--baseline 须在库中已冻结；compare 有出错 trial 时结论 INCOMPLETE，
// --fail-on-unreleasable 时不可放行退出码 1。
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/app"
	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/grader/code"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/repository"
)

const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	defaultDir  = "eval/suites"
	defaultTax  = "eval/taxonomy.yaml"
	defaultOut  = "eval/out"
	defaultRepo = "."
	// defaultBaselines 是 --baseline 写 yaml 的目录（入库）。
	defaultBaselines = "eval/baselines"
	// contractsDir 是 --repo 下 L1 冻结契约目录（schema_valid 用）。
	contractsDir = "contracts"
	// toolPolicyFile 是 --repo 下工具策略（no_model_actuate 取其 actuate 集合，与生产同一份）。
	toolPolicyFile = "policy/tool_policy.yaml"
	usageString    = "usage: eval <validate|run|kbdump|graduate|baseline|compare|aa|resume|promptcheck|judge> [flags]"
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
	case "graduate":
		err = cmdGraduate(ctx, args[1:], stdout, stderr)
	case "baseline":
		err = cmdBaseline(ctx, args[1:], stdout, stderr)
	case "compare":
		err = cmdCompare(ctx, args[1:], stdout, stderr)
	case "aa":
		err = cmdAA(ctx, args[1:], stdout, stderr)
	case "resume":
		err = cmdResume(ctx, args[1:], stdout, stderr)
	case "promptcheck":
		err = cmdPromptCheck(ctx, args[1:], stdout)
	case "judge":
		err = cmdJudge(ctx, args[1:], stdout, stderr)
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
	// holdout 非 nil 时连 holdout 一起加载（authorizeHoldout 签发；P2 L2）。
	holdout *sample.HoldoutGrant
}

func (f *sampleFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.dir, "suites-dir", defaultDir, "样本根目录（递归加载 **.yaml）")
	fs.StringVar(&f.taxonomy, "taxonomy", defaultTax, "question_taxonomy 文件")
	fs.StringVar(&f.repo, "repo", defaultRepo, "仓库根目录（其下 knowledge/ 为知识库；knowledge_ref 为 git ref 时在此仓库取）")
}

// loadedSamples 是通过静态校验的样本及校验所用的 taxonomy / 知识库 / 打分器；refNotes 为参考解不判定维度说明。
type loadedSamples struct {
	samples []*sample.Sample
	tx      *knowledge.Taxonomy
	kb      *knowledge.Store
	// kbContent 是快照 knowledge_ref 内容哈希用的读取器（knowledge.NewTrackedRepoReader）：WORKTREE 只算 git 已跟踪文件、
	// 与 git 读法口径一致，遇符号链接报错。kb 本身仍用 NewRepoReader 加载（与生产一致）。
	kbContent knowledge.TreeReader
	graders   []grader.Grader
	refNotes  []string
	// judgeVersions 是 run --judges 追加的 judge 版本（P2 L2），写快照 judge_versions。
	judgeVersions map[string]string
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
	// P1 L6：degraded_matrix 取 --repo 下的故障矩阵与文案表（与生产同一份）。
	faults, err := app.LoadFaults(f.repo)
	if err != nil {
		return nil, err
	}
	graders := append(code.All(schema, tp.Actuate()), code.NewDegradedMatrix(faults))
	ref := code.NewReferenceCheck(tx, kb, graders)
	rules := append(sample.DefaultRules(tx, kb), ref)
	samples, err := sample.LoadAndValidateWith(f.dir, sample.NewValidator(rules...), f.loadOptions())
	if err != nil {
		return nil, err
	}
	return &loadedSamples{samples: samples, tx: tx, kb: kb, kbContent: knowledge.NewTrackedRepoReader(f.repo), graders: graders, refNotes: ref.Notes()}, nil
}

func cmdValidate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	var sf sampleFlags
	var cf coverageFlags
	sf.register(fs)
	cf.register(fs)
	withHoldout := fs.Bool("holdout", false, "连 holdout 一起校验并计入覆盖报表（须 "+envEvalReader+" 在 "+defaultHoldoutReaders+" 名单中）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *withHoldout {
		if err := sf.authorizeHoldout(); err != nil {
			return err
		}
	}
	ls, err := sf.loadValidSamples()
	if err != nil {
		return err
	}
	samples := ls.samples
	refused, quarantined := 0, 0
	for _, s := range samples {
		if s.IsRefusal() {
			refused++
		}
		if _, ok := s.QuarantineReason(); ok {
			quarantined++
		}
	}
	// 隔离样本仍参与静态校验（计入 samples），但 run 不运行它们（P1-L4 审查修复）。
	fmt.Fprintf(stdout, "validate OK: %d samples, %d refused (%.0f%%), %d quarantined (run skips them)\n",
		len(samples), refused, 100*float64(refused)/float64(len(samples)), quarantined)
	if len(ls.refNotes) > 0 {
		fmt.Fprintf(stdout, "reference not judged: %s\n", strings.Join(ls.refNotes, "; "))
	}
	// P2 L1-T：覆盖报表与补写清单；覆盖不足只报告，--strict-coverage 时才失败。
	return cf.printCoverage(stdout, ls, sf)
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	suites := fs.String("suite", "regression", "套件，逗号分隔")
	n := fs.Int("n", evalkit.DefaultTrials, "每个样本运行次数")
	tags := fs.String("tags", "", "只回放带任一 tag 的样本，逗号分隔，如 L2,protect")
	outRoot := fs.String("out", defaultOut, "报告输出根目录")
	baseline := fs.String("baseline", "", "非空时把报告与配置快照上传 report/{exp_id}/ 并写 {baselines-dir}/{name}.yaml（要求 n >= 3）")
	baselinesDir := fs.String("baselines-dir", defaultBaselines, "baseline yaml 目录")
	trigger := fs.String("trigger", store.TriggerManual, "触发方式 manual|pr|nightly|release（写 experiment.trigger_kind）")
	mode := fs.String("mode", "", "预设 smoke|nightly|release（固定 suite / n / tags / trigger，见 mode.go）；空 = 按 --suite / --n / --tags")
	smokeList := fs.String("smoke-list", defaultSmokeList, "--mode smoke 的冒烟清单")
	budget := fs.Int("token-budget", 0, "累计 token 上限，超出停止派发并置 BUDGET_EXCEEDED；0 = 不限（写 NULL，仅 manual 可用）")
	flagsArg := fs.String("flags", "", flagsUsage)
	var inf infraFlags
	inf.register(fs)
	var jf judgeFlags
	jf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkModeFlags(fs, *mode); err != nil {
		return err
	}
	if *mode != "" {
		*trigger = runModes[*mode].trigger
	}
	if err := checkTrigger(*mode, *trigger); err != nil {
		return err
	}
	if err := checkBudget(*trigger, *budget); err != nil {
		return err
	}
	if *n < 1 {
		return fmt.Errorf("n must be >= 1, got %d", *n)
	}
	if *baseline != "" {
		if err := report.ValidateBaselineName(*baseline); err != nil {
			return err
		}
	}
	runFlags, err := parseEvalFlags(*flagsArg)
	if err != nil {
		return err
	}

	// P2 L2 / L7-a：显式点名 holdout 或 --mode release（全部套件须含 holdout）时才校验读者身份并读入 holdout。
	if wantsHoldout(splitList(*suites)) || *mode == modeRelease {
		if err := sf.authorizeHoldout(); err != nil {
			return err
		}
	}
	ls, err := sf.loadValidSamples()
	if err != nil {
		return err
	}
	if err := jf.attach(ls); err != nil {
		return err
	}
	selected, opts, err := selectRun(*mode, *smokeList, ls,
		evalkit.Options{Suites: splitList(*suites), Tags: splitList(*tags), N: *n, TokenBudget: *budget})
	if err != nil {
		return err
	}
	if *baseline != "" && opts.N < evalkit.DefaultTrials {
		return fmt.Errorf("baseline needs n >= %d (Gate 0 pass^%d), got %d", evalkit.DefaultTrials, evalkit.DefaultTrials, opts.N)
	}
	if len(selected) == 0 {
		return fmt.Errorf("no sample matches suite=%v tags=%v", opts.Suites, opts.Tags)
	}

	in, err := inf.open(ctx, stderr, ls)
	if err != nil {
		return fmt.Errorf("eval infra unavailable (需先 docker compose up 并 make migrate migrate-eval): %w", err)
	}
	// baseline 上传后还要写 experiment.report_key，故连接在函数末尾才关。
	defer func() { err = errors.Join(err, in.close()) }()
	if err := in.setFlags(runFlags); err != nil {
		return err
	}
	opts.Concurrency = in.pool.Size()
	exp, id, err := runWithStore(ctx, in, ls, selected, opts, *trigger)
	if err != nil {
		return err
	}
	// transcript 先于报告上传：报告明细的 run_id 列链到对象键。
	uploaded, err := report.UploadTranscripts(ctx, in.objects, in.evalBucket, exp)
	if err != nil {
		return err
	}
	// P2 L2：holdout 明细只进 report/holdout/ 前缀；本地与公开报告里 holdout 只给 ID 与打分器名。
	full := exp
	exp, path, err := writeReport(ctx, in.objects, in.evalBucket, *outRoot, exp, stdout)
	if err != nil {
		return err
	}
	// P2 L3：nightly 写 metrics.json 与下钻页（其他触发方式不写）。
	if err := nightlyQuality(ctx, in.quality, in.evalBucket, *trigger, ls, &jf, full, exp, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "eval done: exp_id=%s samples=%d trials=%d quarantined=%d report=%s transcripts=%d bucket=%s status=%s\n",
		exp.ID, exp.Samples, len(exp.Trials), len(exp.Quarantined), path, uploaded, in.evalBucket, exp.Gate.Status)
	// 门禁不过（VOID / BUDGET_EXCEEDED / CI 触发下红线 pass^3 < 1）：报告照写，退出非 0，不发布 baseline。
	if err := gateVerdict(*trigger, exp); err != nil {
		return err
	}
	if *baseline == "" {
		return nil
	}
	yamlPath, b, err := report.PublishBaseline(ctx, in.objects, in.evalBucket, *baselinesDir, *baseline, exp, path, clock.System{}.Now())
	if err != nil {
		return err
	}
	if err := store.New(in.evalDB).SetReportKey(ctx, id, b.ReportKey); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "baseline %s: %s report=%s snapshot=%s model=%s\n", b.Name, yamlPath, b.ReportKey, b.SnapshotKey, b.Model)
	return nil
}

// runWithStore 先在 agri_eval.experiment 建行（带配置快照）并把样本清单写 MinIO {id}/manifest.json，
// 回放并跑全部打分器后同事务写 trial、回填快照结果字段并置 DONE；返回实验与 experiment.id。
// 建行之后任一步失败（含 panic）都把实验置 FAILED（store.Fail），不让它停在 RUNNING，见 recordRun。
func runWithStore(ctx context.Context, in *infra, ls *loadedSamples, samples []*sample.Sample, opts evalkit.Options,
	trigger string) (*evalkit.Experiment, uint64, error) {
	wall := clock.System{}
	snap, err := snapshot(ctx, in, ls, samples, opts)
	if err != nil {
		return nil, 0, err
	}
	// runner 先于建行装配：装配失败不留实验行。
	runner, err := newRunner(in, ls, wall.Now)
	if err != nil {
		return nil, 0, err
	}
	replay := func(ctx context.Context, id uint64) (*evalkit.Experiment, error) {
		opts.ExperimentID = store.FormatID(id)
		exp, err := runner.Run(ctx, samples, opts)
		if err != nil {
			return nil, err
		}
		exp.Snapshot = withResults(snap, exp)
		return exp, nil
	}
	exps := gatedExperiments{s: store.New(in.evalDB), budget: opts.TokenBudget, maxRate: in.eval.VoidInfraErrorRate, now: wall.Now}
	return recordRun(ctx, exps, in.objects, in.evalBucket, trigger, snap, evalkit.SampleRefs(samples), wall.Now, replay)
}

// newRunner 装配回放被测 agent 的 Runner（run 与 resume 共用）。
func newRunner(in *infra, ls *loadedSamples, now func() time.Time) (*evalkit.Runner, error) {
	return evalkit.NewRunner(evalkit.PoolEnv(in.pool, in.deps), now,
		evalkit.WithQuestion(in.question), evalkit.WithGraders(ls.kb, ls.graders...),
		evalkit.WithApprover(evalkit.ScriptApproverByRole(in.approvers[0], in.approvers[1], in.approversByRole)), evalkit.WithDevices(in.devices),
		// P1-L4 合并统一：回执步骤用与 Deps.ReceiptSecret 相同的进程内密钥签名，排空复用 DrainOutbox。
		evalkit.WithReceiptSteps(evalkit.ReceiptSteps{Secret: in.receiptSecret, Drain: evalkit.DrainStep}),
		evalkit.WithRawObjects(in.getter, in.rawBucket)) // P2 L6 多轮会话样本
}

// experimentLifecycle 是 run 用到的实验状态写入（store.Experiments 实现；单测替换以覆盖失败路径）。
type experimentLifecycle interface {
	Start(ctx context.Context, trigger string, snap evalkit.Snapshot, at time.Time) (uint64, error)
	Finish(ctx context.Context, id uint64, exp *evalkit.Experiment) error
	Fail(ctx context.Context, id uint64, at time.Time) error
}

// recordRun 建行写清单后执行 replay 并 Finish。建行之后 replay 出错、Finish 出错都置 FAILED；replay panic 时
// 先置 FAILED（结果写 stderr）再用原值重新 panic，不吞原 panic。
func recordRun(ctx context.Context, exps experimentLifecycle, obj repository.ObjectPutter, bucket, trigger string,
	snap evalkit.Snapshot, refs []string, now func() time.Time,
	replay func(ctx context.Context, id uint64) (*evalkit.Experiment, error)) (_ *evalkit.Experiment, _ uint64, err error) {
	id, err := startExperiment(ctx, exps, obj, bucket, trigger, snap, refs, now)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		if r := recover(); r != nil {
			// markFailed 恒返回非 nil（原因或置失败错误），写 stderr 后原值重新 panic。
			fmt.Fprintln(os.Stderr, "eval:", markFailed(ctx, exps, id, fmt.Errorf("panic: %v", r), now))
			panic(r)
		}
		if err != nil {
			err = markFailed(ctx, exps, id, err, now)
		}
	}()
	exp, err := replay(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if err := exps.Finish(ctx, id, exp); err != nil {
		return nil, 0, err
	}
	return exp, id, nil
}

// startExperiment 建 RUNNING 实验行并写样本清单；清单写失败时把实验置 FAILED 后返回错误。
func startExperiment(ctx context.Context, exps experimentLifecycle, obj repository.ObjectPutter, bucket, trigger string,
	snap evalkit.Snapshot, refs []string, now func() time.Time) (uint64, error) {
	id, err := exps.Start(ctx, trigger, snap, now())
	if err != nil {
		return 0, err
	}
	if _, err := store.PutManifest(ctx, obj, bucket, id, snap.SamplesHash, refs); err != nil {
		return 0, markFailed(ctx, exps, id, err, now)
	}
	return id, nil
}

// failTimeout 是置 FAILED 的上限：上下文不随 ctx 取消（中断后仍能落库），但库挂起时不无限等。
const failTimeout = 10 * time.Second

// markFailed 把实验置 FAILED 并返回原错误（置失败本身出错时一并返回）。
func markFailed(ctx context.Context, exps experimentLifecycle, id uint64, cause error, now func() time.Time) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failTimeout)
	defer cancel()
	if err := exps.Fail(fctx, id, now()); err != nil {
		return errors.Join(cause, fmt.Errorf("mark experiment %d FAILED: %w", id, err))
	}
	return fmt.Errorf("experiment %d marked FAILED: %w", id, cause)
}

// snapshot 采集配置快照（评测设计 §12.1、P2 §6.1）：git、提示词版本、模型档位与 provider、知识内容哈希、样本集哈希、
// 加载 / 检索策略、judge 版本（未开 --judges 为空表）、运行参数 suites / tags / n 与作废阈值（resume 沿用）。
func snapshot(ctx context.Context, in *infra, ls *loadedSamples, samples []*sample.Sample, opts evalkit.Options) (evalkit.Snapshot, error) {
	sha, err := gitSHA(ctx)
	if err != nil {
		return evalkit.Snapshot{}, err
	}
	dirty, err := gitDirty(ctx)
	if err != nil {
		return evalkit.Snapshot{}, err
	}
	kref, err := evalkit.KnowledgeRef(ctx, ls.kbContent, samples, knowledge.RefWorktree)
	if err != nil {
		return evalkit.Snapshot{}, err
	}
	return withFlags(evalkit.Snapshot{GitSHA: sha, GitDirty: dirty, PromptVersion: in.model.PromptVersion,
		ModelTier: in.model.DefaultTier, Model: in.model.Provider,
		KnowledgeRef: kref, SamplesHash: evalkit.SamplesHash(samples),
		LoaderStrategy: evalkit.LoaderV0, RetrievalStrategy: evalkit.RetrievalProgressive, JudgeVersions: ls.judgeVersionMap(),
		Suites: opts.Suites, Tags: recordedTags(opts.Tags), N: opts.N, VoidInfraErrorRate: in.eval.VoidInfraErrorRate}, in.flags), nil
}

// recordedTags 是写进快照的样本筛选条件：没有 --tags 时记空表（非 nil），与「快照没记录 tags」（nil，旧实验）区分。
func recordedTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return slices.Clone(tags)
}

// withResults 回填快照的运行结果：token_used 为全部 trial 的 LLM token 合计（桩模型为 0），
// cost 在 L7-b 价目表接入前记 0；wall_time 由 store.Finish 按 started_at / finished_at 计算。
func withResults(snap evalkit.Snapshot, exp *evalkit.Experiment) evalkit.Snapshot {
	snap.TokenUsed = reduce.Reduce(exp.Trials).Metrics.Tokens
	snap.Cost = 0
	return snap
}

func gitSHA(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitDirty 报告工作区是否有未提交改动（含未跟踪文件；被 gitignore 的产物不算）。
func gitDirty(ctx context.Context) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "status", "--porcelain").Output()
	if err != nil {
		return false, fmt.Errorf("git status --porcelain: %w", err)
	}
	return len(bytes.TrimSpace(out)) > 0, nil
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
