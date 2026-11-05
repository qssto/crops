package gate

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// 成本与延迟指标名（eval/gates/cost_latency.yaml 的 metrics[].name）。
const (
	MetricTokensPerSession = "tokens_per_session"
	MetricLatencyP95       = "latency_p95"
	MetricCompactionRate   = "compaction_rate"
)

// ErrBadCostLimits 表示 cost_latency.yaml 不完整或不合法（缺项、重复、未知指标、阈值为空或为负；失败关闭）。
var ErrBadCostLimits = errors.New("gate: bad cost_latency limits")

// Usage 是一个实验的成本与延迟用量：Sessions 是 trial 数（一次 trial 即一个会话），Tokens 是全部 LLM 调用的
// prompt + completion 合计（快照 token_used），Compactions 是触发上下文压缩（COMPACTION_APPLIED）的 trial 数
// （nil = 快照没记录，≠ 0），LatencyP95 是未出错 trial 的 P95 延迟。
type Usage struct {
	Sessions, Tokens int
	Compactions      *int
	LatencyP95       time.Duration
}

// UsageOf 由快照与读回的 trial 求用量。
func UsageOf(snap evalkit.Snapshot, trials []evalkit.Trial) Usage {
	return Usage{Sessions: len(trials), Tokens: snap.TokenUsed, Compactions: snap.Compactions,
		LatencyP95: reduce.Reduce(trials).Metrics.LatencyP95}
}

// TokensPerSession 是单会话 token 成本；没有会话为 0。
func (u Usage) TokensPerSession() float64 { return ratioOf(u.Tokens, u.Sessions) }

// CompactionRate 是压缩触发率（没有会话为 0）；ok 为 false 表示快照没记录压缩数。
func (u Usage) CompactionRate() (rate float64, ok bool) {
	if u.Compactions == nil {
		return 0, false
	}
	return ratioOf(*u.Compactions, u.Sessions), true
}

func (u Usage) latencyP95Ms() (float64, bool) { return float64(u.LatencyP95.Milliseconds()), true }

func (u Usage) tokensPerSession() (float64, bool) { return u.TokensPerSession(), true }

func ratioOf(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// costMetric 是一项成本 / 延迟指标：value 取用量上的值（越大越差），ok 为 false 表示该实验没有这项数据。
type costMetric struct {
	name, title string
	value       func(Usage) (v float64, ok bool)
}

// costMetrics 是成本与延迟指标注册表（顺序即报告顺序）。
var costMetrics = []costMetric{
	{MetricTokensPerSession, "单会话 token（prompt + completion）", Usage.tokensPerSession},
	{MetricLatencyP95, "P95 延迟（ms）", Usage.latencyP95Ms},
	{MetricCompactionRate, "压缩触发率", Usage.CompactionRate},
}

// CostLimit 是一项指标的劣化阈值：candidate > baseline × (1 + MaxIncreaseRatio) 即劣化（baseline 为 0 时 candidate > 0
// 即劣化）。ReportOnlyOnStub 为 true 时桩模型下只出报告、不判定（桩模型延迟不反映真实模型）。
type CostLimit struct {
	Name             string   `yaml:"name"`
	MaxIncreaseRatio *float64 `yaml:"max_increase_ratio"`
	ReportOnlyOnStub bool     `yaml:"report_only_on_stub"`
	Note             string   `yaml:"note,omitempty"`
}

// CostLatency 是 eval/gates/cost_latency.yaml（草稿：待真实模型，signed_by 留空）。
type CostLatency struct {
	SignedBy string      `yaml:"signed_by"`
	Notes    []string    `yaml:"notes"`
	Metrics  []CostLimit `yaml:"metrics"`
}

// CostInput 是成本延迟判定的输入；Stub 为候选用桩模型（快照 model = stub）。
type CostInput struct {
	Limits              []CostLimit
	Baseline, Candidate Usage
	Stub                bool
}

// CostRow 是报告里的一行成本 / 延迟对比。BaselineMissing 为基线没有这项数据（旧快照）：不参与判定、报告写「基线无数据」；
// CandidateMissing 为候选没有数据（基线有时失败关闭，按劣化需签字）。两边都有数据时才算 Limit / Worse。
type CostRow struct {
	Name, Title                       string
	Baseline, Candidate               float64
	Limit                             float64
	BaselineMissing, CandidateMissing bool
	ReportOnly                        bool
	Worse                             bool
}

// LoadCostLatency 严格解析 cost_latency.yaml（未知字段报错）并校验阈值完整。
func LoadCostLatency(path string) (*CostLatency, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gate: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c CostLatency
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("gate: parse %s: %w", path, err)
	}
	if err := checkLimits(c.Metrics); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// checkLimits 要求 costMetrics 每项恰好一条、阈值非空且 ≥ 0，不许未知指标。
func checkLimits(limits []CostLimit) error {
	seen := map[string]bool{}
	for _, l := range limits {
		switch {
		case !slices.ContainsFunc(costMetrics, func(m costMetric) bool { return m.name == l.Name }):
			return fmt.Errorf("%w: unknown metric %q", ErrBadCostLimits, l.Name)
		case seen[l.Name]:
			return fmt.Errorf("%w: %s listed twice", ErrBadCostLimits, l.Name)
		case l.MaxIncreaseRatio == nil || *l.MaxIncreaseRatio < 0:
			return fmt.Errorf("%w: %s max_increase_ratio must be >= 0", ErrBadCostLimits, l.Name)
		}
		seen[l.Name] = true
	}
	for _, m := range costMetrics {
		if !seen[m.name] {
			return fmt.Errorf("%w: missing %s", ErrBadCostLimits, m.name)
		}
	}
	return nil
}

// CostRows 按注册表顺序算两边对比；阈值不完整返回 ErrBadCostLimits。
func CostRows(c CostInput) ([]CostRow, error) {
	if err := checkLimits(c.Limits); err != nil {
		return nil, err
	}
	rows := make([]CostRow, 0, len(costMetrics))
	for _, m := range costMetrics {
		i := slices.IndexFunc(c.Limits, func(l CostLimit) bool { return l.Name == m.name })
		l := c.Limits[i]
		b, bok := m.value(c.Baseline)
		cv, cok := m.value(c.Candidate)
		row := CostRow{Name: m.name, Title: m.title, Baseline: b, Candidate: cv, BaselineMissing: !bok, CandidateMissing: !cok,
			ReportOnly: l.ReportOnlyOnStub && c.Stub}
		if bok && cok {
			row.Limit = b * (1 + *l.MaxIncreaseRatio)
			row.Worse = cv > row.Limit
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// checkCostLatency：任一非只报告的指标劣化超阈值 → NEED_SIGNOFF。基线无数据的指标不参与判定；
// 基线有、候选无数据时无法证明没劣化，同样需签字（失败关闭）。
func checkCostLatency(in *Input) ([]string, error) {
	rows, err := CostRows(in.Cost)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		switch {
		case r.BaselineMissing || r.ReportOnly:
		case r.CandidateMissing:
			out = append(out, fmt.Sprintf("%s: candidate has no data (baseline %.3f)", r.Name, r.Baseline))
		case r.Worse:
			out = append(out, fmt.Sprintf("%s: %.3f → %.3f exceeds %.3f", r.Name, r.Baseline, r.Candidate, r.Limit))
		}
	}
	return out, nil
}
