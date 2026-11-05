package compare

import (
	"sort"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// sampleIndex 是一边在某指标上的样本级值（只含配对交集内的样本）。
func sampleIndex(m reduce.QualityMetric, trials []evalkit.Trial, paired map[string]bool) map[string]reduce.SampleValue {
	out := map[string]reduce.SampleValue{}
	for _, v := range reduce.SampleValues(m, trials) {
		if paired[v.Key] {
			out[v.Key] = v
		}
	}
	return out
}

// metricResult 在配对交集上算一个指标：样本级值配对差（candidate − baseline）的 bootstrap 区间与结论。
func metricResult(m reduce.QualityMetric, in Input, paired map[string]bool, seed uint64) MetricResult {
	res := MetricResult{Name: m.Name(), Title: m.Title(), LowerIsBetter: m.LowerIsBetter(), Pending: m.Pending(),
		NotGating: m.Judge()}
	if res.Pending != "" {
		res.Verdict = VerdictPending
		return res
	}
	b := sampleIndex(m, in.Baseline.Trials, paired)
	c := sampleIndex(m, in.Candidate.Trials, paired)
	keys := commonKeys(b, c)
	res.Samples = len(keys)
	if len(keys) == 0 {
		res.Verdict = VerdictNoApplying
		return res
	}
	bv, cv := values(b, keys), values(c, keys)
	res.BaselineMean, res.CandidateMean = mean(bv), mean(cv)
	res.Diff = PairedBootstrap(subtract(cv, bv), in.Resamples, seed)
	res.Verdict = verdict(res.Diff, res.LowerIsBetter)
	for _, k := range keys {
		if b[k].Flaky || c[k].Flaky {
			res.Flaky = append(res.Flaky, k)
		}
	}
	if in.Reference != nil {
		ref := sampleIndex(m, in.Reference.Trials, paired)
		if three := commonKeys(ref, b, c); len(three) > 0 {
			rv := values(ref, three)
			res.ReferenceMean = mean(rv)
			know := PairedBootstrap(subtract(rv, values(b, three)), in.Resamples, seed)
			agent := PairedBootstrap(subtract(values(c, three), rv), in.Resamples, seed)
			res.Knowledge, res.Agent = &know, &agent
		}
	}
	return res
}

// verdict：区间含 0 为无显著差异；否则按方向（LowerIsBetter 时差值为负才是改善）。
func verdict(iv Interval, lowerIsBetter bool) Verdict {
	if !iv.Significant() {
		return VerdictNoSig
	}
	if (iv.Low > 0) != lowerIsBetter {
		return VerdictImproved
	}
	return VerdictRegressed
}

// commonKeys 返回所有索引都有的样本键（排序）。
func commonKeys(first map[string]reduce.SampleValue, rest ...map[string]reduce.SampleValue) []string {
	keys := make([]string, 0, len(first))
outer:
	for k := range first {
		for _, m := range rest {
			if _, ok := m[k]; !ok {
				continue outer
			}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func values(idx map[string]reduce.SampleValue, keys []string) []float64 {
	out := make([]float64, len(keys))
	for i, k := range keys {
		out[i] = idx[k].Value
	}
	return out
}

func subtract(a, b []float64) []float64 {
	out := make([]float64, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}
