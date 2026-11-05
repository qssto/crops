package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"agri-steward/internal/clock"
	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

var (
	// errResumeSamples 表示实验清单里的 sample_id@version 在当前样本集中缺失（样本已改版或删除）。
	errResumeSamples = errors.New("resume: manifest samples not in --suites-dir")
	// errResumeSnapshot 表示当前配置与实验快照的可比因子或 samples_hash 不一致（补跑会混入不同配置的 trial）。
	errResumeSnapshot = errors.New("resume: current config differs from the experiment snapshot")
	// errNothingToResume 表示实验没有缺失或 INFRA_ERROR 的 trial。
	errNothingToResume = errors.New("resume: no missing or infra_error trial")
	// errResumeBudget 表示实验已用完 token_budget，补跑无预算可用。
	errResumeBudget = errors.New("resume: token_budget already used up")
	// errResumeHoldout 表示实验快照的套件含 holdout，但本次 resume 未经 --holdout 授权读入 holdout。
	errResumeHoldout = errors.New("resume: experiment includes the holdout suite, rerun with --holdout as an authorized reader")
)

// cmdResume 补跑实验中缺失与 INFRA_ERROR 的 trial（按 (sample_id, trial_index)）写回同一实验，再重判作废 / 预算并出报告。
// 失败关闭：冻结 / RUNNING 拒绝；样本清单取 MinIO {id}/manifest.json，须全部在当前样本集；可比因子与 samples_hash 须与快照一致。
func cmdResume(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	var sf sampleFlags
	sf.register(fs)
	expID := fs.Uint64("exp", 0, "要补跑的 experiment id")
	outRoot := fs.String("out", defaultOut, "报告输出根目录")
	withHoldout := fs.Bool("holdout", false, "实验含 holdout 套件（如 release）时须带：校验读者身份并读入 holdout（须 "+envEvalReader+" 在 "+defaultHoldoutReaders+" 名单中）")
	var inf infraFlags
	inf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *expID == 0 {
		return errors.New("resume needs --exp <id>")
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
	in, err := inf.open(ctx, stderr, ls)
	if err != nil {
		return fmt.Errorf("eval infra unavailable (需先 docker compose up 并 make migrate migrate-eval): %w", err)
	}
	defer func() { err = errors.Join(err, in.close()) }()
	exp, trigger, err := resumeExperiment(ctx, in, ls, *expID, sf.holdout != nil)
	if err != nil {
		return err
	}
	// 与 run 同一出报告步骤：holdout 明细进 report/holdout/，本地报告只写脱敏副本。
	full := exp
	exp, path, err := writeReport(ctx, in.objects, in.evalBucket, *outRoot, exp, stdout)
	if err != nil {
		return err
	}
	// P2 L3：补跑的 nightly 同样重写 metrics.json 与下钻页（resume 不带 --judges，judge 指标按未校准）；
	// latest.json 只前进，补跑旧实验不回指。
	if err := nightlyQuality(ctx, in.quality, in.evalBucket, trigger, ls, &judgeFlags{}, full, exp, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "eval resume done: exp_id=%s samples=%d trials=%d report=%s status=%s\n",
		exp.ID, exp.Samples, len(exp.Trials), path, exp.Gate.Status)
	return gateVerdict(trigger, exp)
}

// resumeExperiment 校验可补跑后跑补跑键、写回并重判，返回合并后的完整实验（供报告）与实验 trigger；
// holdoutGranted 为本次是否已授权读入 holdout。
func resumeExperiment(ctx context.Context, in *infra, ls *loadedSamples, id uint64, holdoutGranted bool) (*evalkit.Experiment, string, error) {
	exps := store.New(in.evalDB)
	rec, err := exps.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if err := store.RequireResumable(rec); err != nil {
		return nil, "", err
	}
	if rec.Snapshot == nil {
		return nil, "", fmt.Errorf("%w: experiment %d has no snapshot", errResumeSnapshot, id)
	}
	if err := requireHoldoutGrant(rec.Snapshot.Suites, holdoutGranted); err != nil {
		return nil, "", fmt.Errorf("experiment %d: %w", id, err)
	}
	m, err := store.GetManifest(ctx, in.getter, in.evalBucket, id)
	if err != nil {
		return nil, "", err
	}
	samples, err := pickManifest(ls.samples, m.Samples)
	if err != nil {
		return nil, "", err
	}
	recorded := *rec.Snapshot
	// P2 第 3 批：按快照 flags 重设开关（补跑 trial 与原 trial 同一开关状态）；快照无 flags 时不加。
	if err := in.setFlags(recorded.Flags); err != nil {
		return nil, "", fmt.Errorf("experiment %d: %w", id, err)
	}
	opts := evalkit.Options{ExperimentID: store.FormatID(id), Suites: recorded.Suites, Tags: recorded.Tags, N: recorded.N,
		Concurrency: in.pool.Size()}
	cur, err := snapshot(ctx, in, ls, samples, opts)
	if err != nil {
		return nil, "", err
	}
	if err := checkResumeSnapshot(recorded, cur, m.SamplesHash); err != nil {
		return nil, "", err
	}
	kept, err := exps.Trials(ctx, id)
	if err != nil {
		return nil, "", err
	}
	keys := resumeKeys(samples, opts.N, kept)
	if len(keys) == 0 {
		return nil, "", fmt.Errorf("%w: experiment %d", errNothingToResume, id)
	}
	gate, err := exps.GateOf(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if opts.TokenBudget, err = remainingBudget(gate.TokenBudget, recorded.TokenUsed); err != nil {
		return nil, "", fmt.Errorf("experiment %d: %w", id, err)
	}
	runner, err := newRunner(in, ls, clock.System{}.Now)
	if err != nil {
		return nil, "", err
	}
	fresh, err := runner.RunTrials(ctx, samples, opts, keys)
	if err != nil {
		return nil, "", err
	}
	// 快照沿用实验原值（含 git_sha 与作废阈值），只累加补跑消耗的 token；wall_time 由 store.Resume 按原 started_at 重算。
	fresh.Snapshot = recorded
	fresh.Snapshot.TokenUsed += reduce.Reduce(fresh.Trials).Metrics.Tokens
	st, err := commitResume(ctx, exps, in.objects, in.evalBucket, id, fresh)
	if err != nil {
		return nil, "", err
	}
	full := *fresh
	full.StartedAt, full.Trials = rec.StartedAt, mergeTrials(samples, kept, fresh.Trials)
	full.Gate = evalkit.GateStatus{Status: st.Status, VoidReason: st.VoidReason, Hits: st.Hits,
		MaxInfraErrorRate: recorded.VoidInfraErrorRate, TokenBudget: budgetOf(gate.TokenBudget)}
	return &full, rec.TriggerKind, nil
}

// resumeWriter 是 commitResume 依赖的写库操作（store.Experiments.Resume）。
type resumeWriter interface {
	Resume(ctx context.Context, id uint64, exp *evalkit.Experiment, budgetExceeded bool, maxInfraRate float64) (store.Settlement, error)
}

// commitResume 先在一个事务内写回补跑 trial 并按快照阈值定终态，事务成功后再上传 transcript：事务失败不留对象。
// 只上传终态（非 INFRA_ERROR）trial：终态行不会再被 resume 替换（uk_trial 冲突使后来者整体回滚），每个键只有一个
// 提交成功的写者，两个并发 resume 不会让 MinIO 的 transcript 与库里的 trial 错位；INFRA_ERROR 行可被下一次 resume
// 替换，其 transcript 不上传。上传失败时库已提交，返回错误，可从 trial 库重放。
func commitResume(ctx context.Context, w resumeWriter, obj report.ObjectPutter, bucket string, id uint64,
	fresh *evalkit.Experiment) (store.Settlement, error) {
	st, err := w.Resume(ctx, id, fresh, fresh.BudgetExceeded, fresh.Snapshot.VoidInfraErrorRate)
	if err != nil {
		return store.Settlement{}, err
	}
	final := *fresh
	final.Trials = nil
	var at []int
	for i, t := range fresh.Trials {
		if t.Status() != evalkit.StatusInfraError {
			final.Trials, at = append(final.Trials, t), append(at, i)
		}
	}
	_, err = report.UploadTranscripts(ctx, obj, bucket, &final)
	for j, i := range at {
		fresh.Trials[i].Transcript = final.Trials[j].Transcript
	}
	if err != nil {
		return store.Settlement{}, fmt.Errorf("experiment %d resumed (status=%s) but transcript upload failed: %w", id, st.Status, err)
	}
	return st, nil
}

// requireHoldoutGrant 要求快照套件含 holdout 的实验必须在授权后补跑（失败关闭：未授权时 holdout 不加载，
// 清单里的 holdout 样本只会报「不在样本集」，这里提前给出原因）。
func requireHoldoutGrant(suites []string, granted bool) error {
	if wantsHoldout(suites) && !granted {
		return errResumeHoldout
	}
	return nil
}

// remainingBudget 是补跑可用的 token 预算：NULL 为不限（0）；已用 ≥ 预算时拒绝（失败关闭）。
func remainingBudget(budget *int64, used int) (int, error) {
	if budget == nil {
		return 0, nil
	}
	left := *budget - int64(used)
	if left <= 0 {
		return 0, fmt.Errorf("%w: token_budget=%d token_used=%d", errResumeBudget, *budget, used)
	}
	return int(left), nil
}

// budgetOf 是报告声明的 token_budget（NULL 记 0 = 不限）。
func budgetOf(budget *int64) int {
	if budget == nil {
		return 0
	}
	return int(*budget)
}

// pickManifest 按清单 sample_id@version 从当前样本集取样本（保持加载顺序）；任一缺失即拒绝。
func pickManifest(pool []*sample.Sample, refs []string) ([]*sample.Sample, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: empty manifest", errResumeSamples)
	}
	want := make(map[string]bool, len(refs))
	for _, r := range refs {
		want[r] = true
	}
	var out []*sample.Sample
	for _, s := range pool {
		ref := fmt.Sprintf("%s@%d", s.ID, s.Version)
		if want[ref] {
			out = append(out, s)
			delete(want, ref)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for r := range want {
			missing = append(missing, r)
		}
		slices.Sort(missing)
		return nil, fmt.Errorf("%w: %s", errResumeSamples, strings.Join(missing, " "))
	}
	return out, nil
}

// checkResumeSnapshot 要求当前快照与实验快照的可比因子全部一致、samples_hash 与快照及清单一致；
// git_sha / git_dirty 与运行结果字段不比（补跑通常在另一次 checkout 上进行，配置以因子为准）。
func checkResumeSnapshot(recorded, current evalkit.Snapshot, manifestHash string) error {
	var diffs []string
	if err := store.CheckVoidRate(recorded.VoidInfraErrorRate); err != nil {
		diffs = append(diffs, fmt.Sprintf("void_infra_error_rate recorded %v: %v", recorded.VoidInfraErrorRate, err))
	}
	for _, d := range compare.Diff(compare.Factors, recorded, current) {
		diffs = append(diffs, fmt.Sprintf("%s %q -> %q", d.Name, d.Baseline, d.Candidate))
	}
	if recorded.SamplesHash != current.SamplesHash || recorded.SamplesHash != manifestHash {
		diffs = append(diffs, fmt.Sprintf("samples_hash %q -> %q (manifest %q)", recorded.SamplesHash, current.SamplesHash, manifestHash))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("%w: %s", errResumeSnapshot, strings.Join(diffs, "; "))
	}
	return nil
}

// resumeKeys 返回要补跑的 (sample_id, trial_index)：非隔离样本 0..n-1 中库里缺失或为 INFRA_ERROR 的键（样本顺序、序号升序）。
func resumeKeys(samples []*sample.Sample, n int, trials []evalkit.Trial) []evalkit.TrialKey {
	have := make(map[evalkit.TrialKey]bool, len(trials))
	for _, t := range trials {
		have[evalkit.TrialKey{SampleID: t.SampleID, Index: t.TrialIndex}] = t.Status() != evalkit.StatusInfraError
	}
	var out []evalkit.TrialKey
	for _, s := range samples {
		if _, q := s.QuarantineReason(); q {
			continue
		}
		for i := 0; i < n; i++ {
			if k := (evalkit.TrialKey{SampleID: s.ID, Index: i}); !have[k] {
				out = append(out, k)
			}
		}
	}
	return out
}

// mergeTrials 合并库中原有 trial 与补跑结果：同键以补跑为准，按样本加载顺序、序号排列；
// 库里读回的 trial 不含 tags / 期望结局，按样本补回（报告按 tag 分层统计用）。
func mergeTrials(samples []*sample.Sample, kept, fresh []evalkit.Trial) []evalkit.Trial {
	pos := make(map[string]int, len(samples))
	for i, s := range samples {
		pos[s.ID] = i
	}
	rerun := make(map[evalkit.TrialKey]bool, len(fresh))
	for _, t := range fresh {
		rerun[evalkit.TrialKey{SampleID: t.SampleID, Index: t.TrialIndex}] = true
	}
	out := slices.Clone(fresh)
	for _, t := range kept {
		if rerun[evalkit.TrialKey{SampleID: t.SampleID, Index: t.TrialIndex}] {
			continue
		}
		if i, ok := pos[t.SampleID]; ok {
			t.Tags, t.Expected = samples[i].Tags, samples[i].Expect.Outcome
		}
		out = append(out, t)
	}
	order := func(id string) int {
		if i, ok := pos[id]; ok {
			return i
		}
		return len(samples)
	}
	slices.SortStableFunc(out, func(a, b evalkit.Trial) int {
		if c := order(a.SampleID) - order(b.SampleID); c != 0 {
			return c
		}
		return a.TrialIndex - b.TrialIndex
	})
	return out
}
