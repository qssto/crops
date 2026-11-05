// Package gate 是 P2 L7-b 的质量门禁（P2 §6.8 技术实现 4）：在单因子 compare 结果之上按注册表顺序判定
// BLOCK_REDLINE → BLOCK_QUALITY → NOT_ATTRIBUTABLE → NEED_SIGNOFF → PASS，结果写 experiment.gate_verdict。
// 全部规则都会求值（报告列出每条命中原因），结论取注册顺序中第一条命中的规则；以后加规则只加 Rules 注册项。
// 配对与 bootstrap 一律复用 compare / reduce，本包不另算区间。
package gate

import (
	"errors"
	"fmt"
	"slices"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
)

// Verdict 是门禁结论（experiment.gate_verdict，varchar(16)）。
type Verdict string

// 门禁结论取值。
const (
	Pass            Verdict = "PASS"
	BlockRedLine    Verdict = "BLOCK_REDLINE"
	BlockQuality    Verdict = "BLOCK_QUALITY"
	NeedSignoff     Verdict = "NEED_SIGNOFF"
	NotAttributable Verdict = "NOT_ATTRIBUTABLE"
)

// Verdicts 是 gate_verdict 的合法取值（store 写入前按此白名单校验）。
var Verdicts = []Verdict{Pass, BlockRedLine, BlockQuality, NeedSignoff, NotAttributable}

// Valid 报告 s 是否是合法的门禁结论。
func Valid(s string) bool { return slices.Contains(Verdicts, Verdict(s)) }

// 规则名（报告与测试用）。
const (
	RuleRedLine         = "red_line"
	RuleFullCoverage    = "full_coverage"
	RuleIncomplete      = "incomplete"
	RuleTrustRegression = "trust_regression"
	RuleNotAttributable = "not_attributable"
	RuleCostLatency     = "cost_latency"
	RuleJudgeUnrated    = "judge_unrated"
)

var (
	// ErrNoCompare 表示没有 compare 结果（门禁只在单因子对比之上判定）。
	ErrNoCompare = errors.New("gate: no compare result")
	// ErrPhase 表示阶段级对比：compare_mode=PHASE 不给门禁结论、不写 gate_verdict。
	ErrPhase = errors.New("gate: phase compare gets no gate verdict")
	// ErrNoThreshold 表示参与判定的可信度指标在 thresholds.yaml 中没有阈值（null 或缺项，失败关闭）。
	ErrNoThreshold = errors.New("gate: trust metric has no threshold in thresholds.yaml")
	// ErrNoGatedSamples 表示排除 user_sim 后候选没有可进红线门禁的 trial：红线 / 全覆盖无从判定，不给结论（失败关闭）。
	ErrNoGatedSamples = errors.New("gate: no red-line gated trials in candidate (all user_sim or empty)")
)

// JudgeDim 是一个 judge 维度的门禁状态：Rated 为校准达标（judges.yaml 已开 + 校准报告区间下限 ≥ 阈值）；
// Why 是状态说明（未评原因，进报告）。
type JudgeDim struct {
	Rated bool
	Why   string
}

// Input 是一次门禁判定的输入。
type Input struct {
	// Compare 是候选对 baseline 的单因子 compare 结果（compare.Run）。
	Compare *compare.Result
	// Candidate 是候选实验的全部 trial（红线与全覆盖判定不限于配对交集）。
	Candidate []evalkit.Trial
	// UserSim 报告 trial 是否属 user_sim 样本（读回的 trial 没有 Tags，由调用方按样本清单判定）；
	// trial 自带 user_sim tag 时总是视为 user_sim。红线与全覆盖判定排除这类样本（§6.8 验收 4）。
	UserSim func(evalkit.Trial) bool
	// Thresholds 是 thresholds.yaml 的 指标名 → 退化阈值（nil 即 yaml null）。
	Thresholds map[string]*float64
	// Judge 是 judge 维度 → 门禁状态；缺项即未评（失败关闭）。
	Judge map[string]JudgeDim
	// Cost 是成本与延迟两边的用量与阈值。
	Cost CostInput
}

// Finding 是一条规则的判定记录：Hit 为命中，Reasons 是命中原因（未命中时可为说明）。
type Finding struct {
	Rule    string
	Verdict Verdict
	Hit     bool
	Reasons []string
}

// Rule 是一条门禁规则：Check 返回命中原因（非空即命中）；出错即整次判定失败（不给结论）。
type Rule struct {
	Name    string
	Verdict Verdict
	Check   func(in *Input) (reasons []string, err error)
}

// Rules 是门禁规则注册表，顺序即优先级（简报 L7-b 第 1 条）：
// 红线 → 质量（全覆盖、数据完整、可信度退化）→ 多因子不可归因 → 需签字（成本延迟、judge 未评）。
// judge 未评排在最后：结论上限 NEED_SIGNOFF，不得为 PASS，但不掩盖前面的阻断。
var Rules = []Rule{
	{Name: RuleRedLine, Verdict: BlockRedLine, Check: checkRedLine},
	{Name: RuleFullCoverage, Verdict: BlockQuality, Check: checkFullCoverage},
	{Name: RuleIncomplete, Verdict: BlockQuality, Check: checkIncomplete},
	{Name: RuleTrustRegression, Verdict: BlockQuality, Check: checkTrustRegression},
	{Name: RuleNotAttributable, Verdict: NotAttributable, Check: checkNotAttributable},
	{Name: RuleCostLatency, Verdict: NeedSignoff, Check: checkCostLatency},
	{Name: RuleJudgeUnrated, Verdict: NeedSignoff, Check: checkJudgeUnrated},
}

// Decision 是门禁结论与全部规则的判定记录（按注册顺序）。
type Decision struct {
	Verdict  Verdict
	Findings []Finding
}

// Evaluate 按 Rules 顺序判定：全部规则都求值，结论取第一条命中规则的 Verdict，都不命中为 PASS。
// 阶段级对比、缺 compare 结果、任一规则出错（阈值缺失等）都返回错误，不给结论（失败关闭）。
func Evaluate(in Input) (*Decision, error) {
	switch {
	case in.Compare == nil:
		return nil, ErrNoCompare
	case in.Compare.Mode != compare.ModeFactor:
		return nil, fmt.Errorf("%w: mode=%s", ErrPhase, in.Compare.Mode)
	}
	d := &Decision{}
	for _, r := range Rules {
		reasons, err := r.Check(&in)
		if err != nil {
			return nil, fmt.Errorf("gate: rule %s: %w", r.Name, err)
		}
		hit := len(reasons) > 0
		d.Findings = append(d.Findings, Finding{Rule: r.Name, Verdict: r.Verdict, Hit: hit, Reasons: reasons})
		if hit && d.Verdict == "" {
			d.Verdict = r.Verdict
		}
	}
	if d.Verdict == "" {
		d.Verdict = Pass
	}
	return d, nil
}
