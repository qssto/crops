// Package reduce 把一次实验的全部 trial 按样本聚合（评测设计 §8.2-8.3）：
// 红线 pass^k（k = n）、质量指标均值、flaky 清单、unexpected_call 清单、infra_error / grader_error 统计，
// 以及 Gate 0 自动项判定（gate.go）。report 只消费这里的结果，不再自行计数。
package reduce

import (
	"math"
	"sort"
	"time"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// p95 是延迟分位（nearest-rank）。
const p95 = 0.95

// Dim 是一个打分维度：trial 级计数 + 样本级均值。
type Dim struct {
	Name         string
	RedLine      bool
	Pass         int
	Fail         int
	GraderErrors int
	// Mean 先对每个样本内已打分 trial 的 value 取均值，再对样本取均值（§8.2 套件级 = 样本均值）。
	Mean float64
}

// TrialRef 是样本内一次 trial 的定位信息（flaky 清单用来链到 run / transcript）。
type TrialRef struct {
	Index      int
	RunID      uint64
	Status     string
	Transcript string
}

// Sample 是一个样本的 n 次 trial 汇总。出错（INFRA_ERROR / grader_error）的 trial 计入 N、不计通过。
type Sample struct {
	ID string
	N  int
	// Pass 是各维度的通过次数；AllPass 是红线全过的 trial 数。
	Pass    map[string]int
	AllPass int
	Trials  []TrialRef
}

// PassK 是样本红线全过的 pass^k（k = n）。
func (s *Sample) PassK() float64 { return PassHatK(s.N, s.AllPass, s.N) }

// Flaky 报告同一样本的红线结果是否不一致（0 < 全过次数 < n，§8.3）。
func (s *Sample) Flaky() bool { return s.AllPass > 0 && s.AllPass < s.N }

// Flaky 是 flaky 清单的一行：Dims 为结果不一致的红线维度。
type Flaky struct {
	Sample *Sample
	Dims   []string
}

// UnexpectedCall 是一次 trial 中未在 fixtures.mcp 声明的工具调用。
type UnexpectedCall struct {
	SampleID   string
	TrialIndex int
	RunID      uint64
	Tools      []string
}

// ErrorEntry 是一条 infra_error（Dim 为空）或 grader_error。
type ErrorEntry struct {
	SampleID   string
	TrialIndex int
	Dim        string
	Message    string
}

// Ratio 是「分子 / 分母」，分母为 0 时无意义。
type Ratio struct {
	Num, Den int
}

// Value 返回比例；分母为 0 时为 0。
func (r Ratio) Value() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Metrics 是 baseline 指标（P0 §6.9 功能任务 4）；结论一致性人工抽检不在此列。
type Metrics struct {
	// RedLinePassK 是各样本红线全过 pass^k 的均值。
	RedLinePassK float64
	// CitationCoverage：ADVICE trial 中建议卡至少带一条引用的比例。
	CitationCoverage Ratio
	// RefusalRate：未出错 trial 中 REFUSED 的比例。
	RefusalRate Ratio
	// LatencyP95 取未出错 trial；Tokens 累加 Detail 中 LLM 调用的 prompt + completion，TokensRecorded 为是否有 Detail。
	LatencyP95     time.Duration
	Tokens         int
	TokensRecorded bool
}

// Result 是 Reduce 的输出；各切片按首次出现顺序。
type Result struct {
	Trials          int
	Dims            []*Dim
	RedLines        []string
	Samples         []*Sample
	Flaky           []Flaky
	UnexpectedCalls []UnexpectedCall
	InfraErrors     []ErrorEntry
	GraderErrors    []ErrorEntry
	Metrics         Metrics
}

// Void 报告实验是否作废：存在未修复的 grader_error 时 pass^k 不可信（§11.3-4）。
func (r *Result) Void() bool { return len(r.GraderErrors) > 0 }

// Reduce 聚合全部 trial。
func Reduce(trials []evalkit.Trial) *Result {
	r := &Result{Trials: len(trials)}
	r.collectDims(trials)
	r.collectSamples(trials)
	r.collectIssues(trials)
	r.Metrics = metrics(trials, r.Samples)
	return r
}

// collectDims 汇总各维度计数与样本级均值。
func (r *Result) collectDims(trials []evalkit.Trial) {
	idx := map[string]*Dim{}
	type acc struct {
		sum float64
		n   int
	}
	perSample := map[string]map[string]*acc{} // dim → sample → value 累加
	var sampleOrder []string
	seen := map[string]bool{}
	for _, t := range trials {
		if !seen[t.SampleID] {
			seen[t.SampleID] = true
			sampleOrder = append(sampleOrder, t.SampleID)
		}
		for _, sc := range t.Scores {
			d, ok := idx[sc.Name]
			if !ok {
				d = &Dim{Name: sc.Name, RedLine: sc.RedLine}
				idx[sc.Name] = d
				r.Dims = append(r.Dims, d)
				perSample[sc.Name] = map[string]*acc{}
				if sc.RedLine {
					r.RedLines = append(r.RedLines, sc.Name)
				}
			}
			if sc.Error != "" {
				d.GraderErrors++
				continue
			}
			if sc.Pass {
				d.Pass++
			} else {
				d.Fail++
			}
			a, ok := perSample[sc.Name][t.SampleID]
			if !ok {
				a = &acc{}
				perSample[sc.Name][t.SampleID] = a
			}
			a.sum += sc.Value
			a.n++
		}
	}
	for _, d := range r.Dims {
		var sum float64
		var n int
		for _, id := range sampleOrder {
			if a, ok := perSample[d.Name][id]; ok {
				sum += a.sum / float64(a.n)
				n++
			}
		}
		if n > 0 {
			d.Mean = sum / float64(n)
		}
	}
}

// collectSamples 按样本统计红线通过次数并找出 flaky。
func (r *Result) collectSamples(trials []evalkit.Trial) {
	idx := map[string]*Sample{}
	for _, t := range trials {
		s, ok := idx[t.SampleID]
		if !ok {
			s = &Sample{ID: t.SampleID, Pass: map[string]int{}}
			idx[t.SampleID] = s
			r.Samples = append(r.Samples, s)
		}
		s.N++
		s.Trials = append(s.Trials, TrialRef{Index: t.TrialIndex, RunID: t.RunID, Status: t.Status(), Transcript: t.Transcript})
		allPass := t.Error == ""
		for _, d := range r.Dims {
			if dimPassed(t, d.Name) {
				s.Pass[d.Name]++
				continue
			}
			if d.RedLine {
				allPass = false
			}
		}
		if allPass {
			s.AllPass++
		}
	}
	for _, s := range r.Samples {
		if !s.Flaky() {
			continue
		}
		var dims []string
		for _, name := range r.RedLines {
			if c := s.Pass[name]; c > 0 && c < s.N {
				dims = append(dims, name)
			}
		}
		r.Flaky = append(r.Flaky, Flaky{Sample: s, Dims: dims})
	}
}

// dimPassed 报告 trial 在该维度是否通过（运行出错、未打分、grader_error 都不算通过）。
func dimPassed(t evalkit.Trial, name string) bool {
	if t.Error != "" {
		return false
	}
	sc, ok := t.ScoreOf(name)
	return ok && sc.Error == "" && sc.Pass
}

// collectIssues 列出 unexpected_call、infra_error 与 grader_error。
func (r *Result) collectIssues(trials []evalkit.Trial) {
	for _, t := range trials {
		if len(t.UnexpectedCalls) > 0 {
			r.UnexpectedCalls = append(r.UnexpectedCalls, UnexpectedCall{SampleID: t.SampleID, TrialIndex: t.TrialIndex,
				RunID: t.RunID, Tools: t.UnexpectedCalls})
		}
		if t.Error != "" {
			r.InfraErrors = append(r.InfraErrors, ErrorEntry{SampleID: t.SampleID, TrialIndex: t.TrialIndex, Message: t.Error})
		}
		for _, sc := range t.Scores {
			if sc.Error != "" {
				r.GraderErrors = append(r.GraderErrors, ErrorEntry{SampleID: t.SampleID, TrialIndex: t.TrialIndex,
					Dim: sc.Name, Message: sc.Error})
			}
		}
	}
}

// metrics 计算 baseline 指标。
func metrics(trials []evalkit.Trial, samples []*Sample) Metrics {
	var m Metrics
	if len(samples) > 0 {
		var sum float64
		for _, s := range samples {
			sum += s.PassK()
		}
		m.RedLinePassK = sum / float64(len(samples))
	}
	var lat []time.Duration
	for _, t := range trials {
		if t.Detail != nil {
			m.TokensRecorded = true
			for _, st := range t.Detail.Steps {
				for _, l := range st.LLMCalls {
					m.Tokens += l.PromptTokens + l.CompletionTokens
				}
			}
		}
		if t.Error != "" {
			continue
		}
		lat = append(lat, t.Latency)
		m.RefusalRate.Den++
		switch t.Outcome {
		case model.OutcomeRefused:
			m.RefusalRate.Num++
		case model.OutcomeAdvice:
			m.CitationCoverage.Den++
			if t.Card != nil && len(t.Card.Citations) > 0 {
				m.CitationCoverage.Num++
			}
		}
	}
	m.LatencyP95 = percentile(lat, p95)
	return m
}

// percentile 是 nearest-rank 分位；空切片为 0。
func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}

// PassHatK 是 k 次独立运行全部通过的无偏估计 C(c,k)/C(n,k)；c < k 为 0，k 非法为 0。
func PassHatK(n, c, k int) float64 {
	if k < 1 || k > n || c < k {
		return 0
	}
	p := 1.0
	for i := 0; i < k; i++ {
		p *= float64(c-i) / float64(n-i)
	}
	return p
}
