package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/gate"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

const (
	// defaultCostLatency 是成本 / 延迟劣化阈值（草稿：待真实模型，signed_by 留空）。
	defaultCostLatency = "eval/gates/cost_latency.yaml"
)

var (
	// errGateBlocked 是门禁不放行的退出错误：非 PASS，或 NEED_SIGNOFF 未通过签字校验（release 据退出码非 0 拦截）。
	errGateBlocked = errors.New("gate: not releasable")
	// errGateRefused 表示实验不给门禁结论（VOID / BUDGET_EXCEEDED / 未跑完 / 阶段级对比），不写 gate_verdict。
	errGateRefused = errors.New("gate: experiment gets no gate verdict")
	// errGateSuites 表示基线与候选跑的套件不同（不给结论，不写 gate_verdict）。
	errGateSuites = errors.New("gate: suites differ")
)

// gateFlags 是 gate 子命令参数。
type gateFlags struct {
	stf                                  storeFlags
	exp                                  uint64
	baseline, baselinesDir, out          string
	suitesDir, thresholds, costLatency   string
	judges, signoffs, codeowners, owners string
	repo, trustRef                       string
	seed                                 uint64
	resamples                            int
}

func (f *gateFlags) parse(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	f.stf.register(fs)
	fs.Uint64Var(&f.exp, "exp", 0, "候选 experiment.id")
	fs.StringVar(&f.baseline, "baseline", "", "baseline 名（{baselines-dir}/{名}.yaml）或 experiment.id；空 = 读 {baselines-dir}/"+report.ReleasePointer+".yaml 指针")
	fs.StringVar(&f.baselinesDir, "baselines-dir", defaultBaselines, "baseline yaml 目录")
	fs.StringVar(&f.out, "out", defaultOut, "报告输出根目录（写 {out}/gate/{baseline}-{candidate}.md）")
	fs.StringVar(&f.suitesDir, "suites-dir", defaultDir, "样本根目录（取 user_sim 样本 ID：不进红线判定）")
	fs.StringVar(&f.thresholds, "thresholds", defaultThresholds, "可信度指标退化阈值")
	fs.StringVar(&f.costLatency, "cost-latency", defaultCostLatency, "成本 / 延迟劣化阈值")
	fs.StringVar(&f.judges, "judges", review.DefaultGate, "judge 门禁配置")
	fs.StringVar(&f.signoffs, "signoffs-dir", defaultSignoffs, "签字文件目录（相对 --repo）")
	fs.StringVar(&f.codeowners, "codeowners", defaultCodeowners, "CODEOWNERS（相对 --repo）")
	fs.StringVar(&f.owners, "owners", defaultOwners, "CODEOWNERS 账号 → 邮箱 / user_id（相对 --repo）")
	fs.StringVar(&f.repo, "repo", defaultRepo, "仓库根目录（git log 校验签字作者）")
	fs.StringVar(&f.trustRef, "trust-ref", defaultTrustRef, "签字信任 ref：签字文件须已进该 ref，CODEOWNERS / owners 取该 ref 的版本")
	fs.Uint64Var(&f.seed, "bootstrap-seed", compare.DefaultSeed, "配对 bootstrap seed")
	fs.IntVar(&f.resamples, "resamples", compare.DefaultResamples, "bootstrap 重抽次数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if f.exp == 0 {
		return errors.New("gate: --exp is required")
	}
	if f.baseline == "" {
		name, err := releaseBaseline(f.baselinesDir)
		if err != nil {
			return err
		}
		f.baseline = name
	}
	return nil
}

// releaseBaseline 读 release 门禁基线指针 {dir}/release.yaml（严格解码，只有 baseline 一个字段）并返回所指基线名。
// 指针由调度方冻结基线后写入；缺失、格式错、指向非法名 / 纯数字 id / 自身都报错（失败关闭，不退回任何默认基线）。
func releaseBaseline(dir string) (string, error) {
	p := filepath.Join(dir, report.ReleasePointer+".yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("gate: 未给 --baseline 且读不到基线指针（调度方冻结基线后写 %s）: %w", p, err)
	}
	var doc struct {
		Baseline string `yaml:"baseline"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("gate: parse %s: %w", p, err)
	}
	if _, err := strconv.ParseUint(doc.Baseline, 10, 64); err == nil {
		return "", fmt.Errorf("gate: %s baseline %q must be a baseline name, not an experiment id", p, doc.Baseline)
	}
	if err := report.ValidateBaselineName(doc.Baseline); err != nil {
		return "", fmt.Errorf("gate: %s: %w", p, err)
	}
	return doc.Baseline, nil
}

// checkGateSuites 要求基线与候选跑的套件相同（顺序无关）：套件不同时红线 / 质量归约的样本面不同，结论不可信。
func checkGateSuites(b, c evalkit.Snapshot) error {
	bs, cs := sortedList(b.Suites), sortedList(c.Suites)
	if !slices.Equal(bs, cs) {
		return fmt.Errorf("%w: 基线套件 %s 与候选 %s 不一致", errGateSuites, strings.Join(bs, ","), strings.Join(cs, ","))
	}
	return nil
}

func sortedList(xs []string) []string {
	out := slices.Clone(xs)
	slices.Sort(out)
	return out
}

// gateConfig 是判定前读入的配置（先于连库读，配置错即失败，不写库）。
type gateConfig struct {
	thresholds map[string]*float64
	cost       *gate.CostLatency
	judges     *review.Gate
	userSim    map[string]bool
}

func loadGateConfig(f *gateFlags, logW io.Writer) (*gateConfig, error) {
	th, err := loadGateThresholds(f.thresholds)
	if err != nil {
		return nil, err
	}
	cost, err := gate.LoadCostLatency(f.costLatency)
	if err != nil {
		return nil, err
	}
	judges, err := review.LoadGate(f.judges)
	if err != nil {
		return nil, err
	}
	us, err := userSimIDs(f.suitesDir, logW)
	if err != nil {
		return nil, err
	}
	return &gateConfig{thresholds: th, cost: cost, judges: judges, userSim: us}, nil
}

// userSimIDs 是带 user_sim tag 的样本 ID（读回的 trial 没有 tags，按样本清单判定）。解析失败的样本文件不在集合里：
// 只会让更多样本进红线判定（失败关闭）；样本合法性由 validate 负责。
func userSimIDs(dir string, logW io.Writer) (map[string]bool, error) {
	samples, problems, err := sample.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("gate: load samples %s: %w", dir, err)
	}
	if len(problems) > 0 {
		fmt.Fprintf(logW, "gate: %d sample files failed to parse; treated as non user_sim (fail closed)\n", len(problems))
	}
	out := map[string]bool{}
	for _, s := range samples {
		if slices.Contains(s.Tags, sample.TagUserSim) {
			out[s.ID] = true
		}
	}
	return out, nil
}

// userSimPredicate 判定候选 trial 是否属 user_sim 样本：优先用候选 run 快照记录的 user_sim 样本 ID（样本目录之后改 tag
// 不影响已跑实验）；快照没记录（旧实验或本次无 user_sim 样本）才退回样本目录扫描结果 dir。gate 另要求 suite = capability。
func userSimPredicate(snap evalkit.Snapshot, dir map[string]bool) func(evalkit.Trial) bool {
	if snap.UserSim == nil {
		return func(t evalkit.Trial) bool { return dir[t.SampleID] }
	}
	ids := make(map[string]bool, len(snap.UserSim.SampleIDs))
	for _, id := range snap.UserSim.SampleIDs {
		ids[id] = true
	}
	return func(t evalkit.Trial) bool { return ids[t.SampleID] }
}

func cmdGate(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	var f gateFlags
	if err := f.parse(args); err != nil {
		return err
	}
	cfg, err := loadGateConfig(&f, stderr)
	if err != nil {
		return err
	}
	es, err := f.stf.open(ctx, stderr)
	if err != nil {
		return fmt.Errorf("eval store unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, es.close()) }()
	if err := refuseGate(ctx, es.exps, f.exp); err != nil {
		return err
	}
	baseID, err := resolveBaseline(ctx, es.exps, f.baselinesDir, f.baseline)
	if err != nil {
		return err
	}
	in, ci, err := gateInput(ctx, es, &f, cfg, baseID)
	if err != nil {
		return err
	}
	d, err := gate.Evaluate(*in)
	if err != nil {
		return err
	}
	diff, err := gateFactorDiff(in.Compare)
	if err != nil {
		return err
	}
	if err := es.exps.RecordGate(ctx, f.exp, baseID, diff, string(d.Verdict)); err != nil {
		return err
	}
	signed, note := signoff(ctx, es.exps, &f, d.Verdict)
	path, err := writeGateReport(f.out, gate.ReportInput{Decision: d, Compare: in.Compare, Flips: gate.Flips(ci),
		Cost: in.Cost, Signoff: note, Holdout: holdoutIDs(in.Candidate)})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "gate done: baseline=%d candidate=%d verdict=%s signed=%t signoff=%q report=%s\n",
		baseID, f.exp, d.Verdict, signed, note, path)
	return gateExit(d.Verdict, signed)
}

// gateFactorDiff 是写 gate_verdict 同行的 factor_diff：只有单因子对比有（Record ok=false 即没有可记的 factor_diff，
// 不写结论；gateInput 恒用单因子模式，这里兜住以后改动）。
func gateFactorDiff(r *compare.Result) (json.RawMessage, error) {
	diff, ok, err := r.Record()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: compare mode %s has no factor_diff", errGateRefused, r.Mode)
	}
	return diff, nil
}

// refuseGate：VOID / BUDGET_EXCEEDED / 未 DONE / 已登记阶段级对比的实验不给结论（不写 gate_verdict）。
func refuseGate(ctx context.Context, exps *store.Experiments, id uint64) error {
	g, err := exps.GateOf(ctx, id)
	if err != nil {
		return err
	}
	if g.Status != store.StatusDone {
		return fmt.Errorf("%w: exp %d status=%s", errGateRefused, id, g.Status)
	}
	rec, err := exps.Get(ctx, id)
	if err != nil {
		return err
	}
	if rec.CompareMode == store.ComparePhase {
		return fmt.Errorf("%w: exp %d compare_mode=PHASE", errGateRefused, id)
	}
	return nil
}

// gateInput 读两边、做单因子 compare（复用 compare.Run / reduce），组装门禁输入；另返回 compare 输入供翻转清单复用同一配对。
func gateInput(ctx context.Context, es *evalStore, f *gateFlags, cfg *gateConfig, baseID uint64) (
	*gate.Input, compare.Input, error) {
	ci := compare.Input{Mode: compare.ModeFactor, Seed: f.seed, Resamples: f.resamples}
	var err error
	if ci.Baseline, err = loadSide(ctx, es.exps, baseID); err != nil {
		return nil, ci, err
	}
	if ci.Candidate, err = loadSide(ctx, es.exps, f.exp); err != nil {
		return nil, ci, err
	}
	if err := checkGateSuites(ci.Baseline.Snapshot, ci.Candidate.Snapshot); err != nil {
		return nil, ci, fmt.Errorf("%w（baseline %d / candidate %d）", err, baseID, f.exp)
	}
	keys, err := es.exps.PairedSamples(ctx, baseID, f.exp)
	if err != nil {
		return nil, ci, err
	}
	for _, k := range keys {
		ci.Paired = append(ci.Paired, k.String())
	}
	r, err := compare.Run(ci)
	if err != nil {
		if errors.Is(err, compare.ErrIncomparable) {
			return nil, ci, fmt.Errorf("%w（baseline %d / candidate %d）", err, baseID, f.exp)
		}
		return nil, ci, err
	}
	b, c := ci.Baseline, ci.Candidate
	in := &gate.Input{Compare: r, Candidate: c.Trials, Thresholds: cfg.thresholds,
		UserSim: userSimPredicate(c.Snapshot, cfg.userSim),
		Judge:   judgeStatus(ctx, es.getter, es.bucket, cfg.judges, c.Snapshot),
		Cost: gate.CostInput{Limits: cfg.cost.Metrics, Baseline: gate.UsageOf(b.Snapshot, b.Trials),
			Candidate: gate.UsageOf(c.Snapshot, c.Trials), Stub: c.Snapshot.Model == evalkit.ModelStub}}
	return in, ci, nil
}

// signoff 在 NEED_SIGNOFF 时校验签字文件并写 signed_by；返回是否已签与报告说明。校验失败不是命令错误（报告照写，退出非 0）。
func signoff(ctx context.Context, exps *store.Experiments, f *gateFlags, v gate.Verdict) (bool, string) {
	if v != gate.NeedSignoff {
		return false, "无需签字"
	}
	c := signoffConfig{repo: f.repo, trustRef: f.trustRef, dir: f.signoffs, codeowners: f.codeowners, owners: f.owners}
	s, err := verifySignoff(ctx, c, f.exp)
	if err != nil {
		return false, fmt.Sprintf("未签字：%v", err)
	}
	if err := exps.SetSignedBy(ctx, f.exp, s.UserID); err != nil {
		return false, fmt.Sprintf("签字校验通过但写 signed_by 失败：%v", err)
	}
	return true, fmt.Sprintf("已签字：%s（user_id=%d）", s.Handle, s.UserID)
}

// holdoutIDs 是候选 trials 中 holdout 样本 ID（报告只给 ID）。
func holdoutIDs(trials []evalkit.Trial) map[string]bool {
	out := map[string]bool{}
	for _, t := range trials {
		if t.Holdout {
			out[t.SampleID] = true
		}
	}
	return out
}

func writeGateReport(outRoot string, in gate.ReportInput) (string, error) {
	var b strings.Builder
	if err := gate.Render(&b, in); err != nil {
		return "", err
	}
	return writeLocal(filepath.Join(outRoot, "gate"), fmt.Sprintf("%d-%d.md", in.Compare.BaselineID, in.Compare.CandidateID), b.String())
}

// gateExit：PASS、已签字的 NEED_SIGNOFF 退出 0；其余退出非 0。
func gateExit(v gate.Verdict, signed bool) error {
	if v == gate.Pass || (v == gate.NeedSignoff && signed) {
		return nil
	}
	return fmt.Errorf("%w: verdict=%s signed=%t", errGateBlocked, v, signed)
}
