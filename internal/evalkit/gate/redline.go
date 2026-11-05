package gate

import (
	"fmt"
	"slices"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/sample"
)

// TagUserSim 是 LLM 模拟用户样本的 tag（与 sample.TagUserSim 一致）：gate 的红线判定排除此类样本（§6.8 验收 4）。
const TagUserSim = sample.TagUserSim

// SuiteCapability 是 user_sim 样本唯一允许的套件（sample.SuiteCapability）：排除 user_sim 还要求 trial 属此套件。
const SuiteCapability = sample.SuiteCapability

// fullCoverageMetrics 是「低于 100% 无条件 BLOCK_QUALITY」的指标（简报 L7-b 第 1 条：引用覆盖率、数值准确率）。
var fullCoverageMetrics = []string{"citation_coverage", "numeric_accuracy"}

// IsUserSim 报告 trial 是否带 user_sim tag（运行中的 trial 有 Tags；读回的 trial 没有，见 Input.UserSim）。
func IsUserSim(t evalkit.Trial) bool { return slices.Contains(t.Tags, TagUserSim) }

// RedLineFailures 返回红线全过 pass^3 < 1 的样本 id（出错 trial 计入 n、不计通过；n < 3 也算不过，失败关闭）。
// userSim 非 nil 时另按它排除样本；带 user_sim tag 的 trial 总是排除（两者都只限 capability 套件，见 gated）。
// run 的 CI 退出判定与 gate 共用本函数。
func RedLineFailures(trials []evalkit.Trial, userSim func(evalkit.Trial) bool) []string {
	var out []string
	for _, s := range reduce.Reduce(gated(trials, userSim)).Samples {
		if reduce.PassHatK(s.N, s.AllPass, reduce.GateK) < 1 {
			out = append(out, s.ID)
		}
	}
	return out
}

// gated 去掉 user_sim 样本的 trial（红线与全覆盖只看可进红线门禁的样本）。只排除 capability 套件的 trial：
// user_sim 样本只允许在 capability（sample 校验），其他套件里带 tag / 被谓词命中的照常进红线（失败关闭）。
func gated(trials []evalkit.Trial, userSim func(evalkit.Trial) bool) []evalkit.Trial {
	out := make([]evalkit.Trial, 0, len(trials))
	for _, t := range trials {
		if t.Suite == SuiteCapability && (IsUserSim(t) || (userSim != nil && userSim(t))) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// gatedCandidate 是候选可进红线门禁的 trial；为空（全是 user_sim 或没有 trial）返回 ErrNoGatedSamples，
// 不把「没有样本」当作红线 / 全覆盖通过。
func gatedCandidate(in *Input) ([]evalkit.Trial, error) {
	trials := gated(in.Candidate, in.UserSim)
	if len(trials) == 0 {
		return nil, fmt.Errorf("%w: %d candidate trials", ErrNoGatedSamples, len(in.Candidate))
	}
	return trials, nil
}

func checkRedLine(in *Input) ([]string, error) {
	trials, err := gatedCandidate(in)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range RedLineFailures(trials, nil) {
		out = append(out, fmt.Sprintf("%s: red-line pass^%d < 1", id, reduce.GateK))
	}
	return out, nil
}

// checkFullCoverage：候选全部可进红线门禁的样本上，引用覆盖率 / 数值准确率的样本级值（红线指标取最小值）须为 1。
func checkFullCoverage(in *Input) ([]string, error) {
	trials, err := gatedCandidate(in)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range fullCoverageMetrics {
		m, err := qualityMetric(name)
		if err != nil {
			return nil, err
		}
		for _, v := range reduce.SampleValues(m, trials) {
			if v.Value < 1 {
				out = append(out, fmt.Sprintf("%s: %s = %.3f < 1", name, v.Key, v.Value))
			}
		}
	}
	return out, nil
}

// qualityMetric 按名字取 reduce.QualityMetrics 的指标；不存在即报错（注册表改名时失败关闭）。
func qualityMetric(name string) (reduce.QualityMetric, error) {
	for _, m := range reduce.QualityMetrics {
		if m.Name() == name {
			return m, nil
		}
	}
	return nil, fmt.Errorf("gate: quality metric %s not registered in reduce.QualityMetrics", name)
}
