package review

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/store"
)

// MinCalibrationPairs 是可进门禁所需的最少 judge-专家评分对（CALIBRATION 主标、排除 UNKNOWN，L2 评审 P1-3）。
const MinCalibrationPairs = 100

// DefaultGate 是 judge 门禁配置（P2 L2：阈值待实测，三个维度默认关闭、judge 只进报告）。
const DefaultGate = "eval/gates/judges.yaml"

// ErrBadGate 表示门禁配置缺维度、阈值越界或含未知维度。
var ErrBadGate = errors.New("review: bad judge gate config")

// Gate 是 judge 门禁配置：Kappa 区间下限 ≥ KappaThreshold 的维度可进门禁，Enabled 由 PR 打开。
type Gate struct {
	KappaThreshold float64                `yaml:"kappa_threshold"`
	Dimensions     map[string]GateDimItem `yaml:"dimensions"`
}

// GateDimItem 是一个维度的门禁开关。
type GateDimItem struct {
	Enabled bool `yaml:"enabled"`
}

// LoadGate 严格解析门禁配置：三个维度必须齐全，阈值在 (0, 1]。
func LoadGate(path string) (*Gate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("review: read gate: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var g Gate
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("review: parse %s: %w", path, err)
	}
	if g.KappaThreshold <= 0 || g.KappaThreshold > 1 {
		return nil, fmt.Errorf("%w: kappa_threshold %v", ErrBadGate, g.KappaThreshold)
	}
	for d := range g.Dimensions {
		if err := judge.CheckDim(d); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadGate, err)
		}
	}
	for _, d := range judge.Dimensions {
		if _, ok := g.Dimensions[d]; !ok {
			return nil, fmt.Errorf("%w: missing dimension %s", ErrBadGate, d)
		}
	}
	return &g, nil
}

// Report 是一个维度一个 judge 版本的一致性报告（CALIBRATION 或 MONTHLY_RECHECK）。
type Report struct {
	TaskType     string    `json:"task_type"`
	Dimension    string    `json:"dimension"`
	JudgeVersion string    `json:"judge_version"`
	GeneratedAt  time.Time `json:"generated_at"`
	Threshold    float64   `json:"kappa_threshold"`
	// JudgeExpert 是 judge-专家二次加权 Kappa（排除 UNKNOWN、只取主标）；InterExpert 是专家间（可达上限）。
	JudgeExpert  Interval `json:"judge_expert"`
	JudgeMatrix  Matrix   `json:"judge_matrix"`
	InterExpert  Interval `json:"inter_expert"`
	ExpertMatrix Matrix   `json:"expert_matrix"`
	UnknownRatio *float64 `json:"unknown_ratio"`
	// ExpertStrata 是主标专家分数 1/2/3 各层次数（排除 judge UNKNOWN）；Short 为不足 PerStratum 的层（待补抽）。
	ExpertStrata [categories]int `json:"expert_strata"`
	Short        []string        `json:"short,omitempty"`
	// GateReady 表示可进门禁（以 PR 打开 Gate 中该维度）：只对 CALIBRATION，judge-专家 n ≥ MinCalibrationPairs、
	// 区间可用（无定义重抽不超 MaxUndefinedRatio）且下限 ≥ 阈值；月度复核只报一致性，恒为 false。GateReason 是不可进的原因。
	GateReady  bool     `json:"gate_ready"`
	GateReason string   `json:"gate_reason,omitempty"`
	Enabled    bool     `json:"gate_enabled"`
	Notes      []string `json:"notes"`
}

// ReportInput 是 BuildReport 的输入（三条统计查询的结果）。
type ReportInput struct {
	TaskType, Dimension, JudgeVersion string
	JudgeExpert, InterExpert          []store.ScorePair
	UnknownRatio                      float64
	UnknownOK                         bool
	Gate                              *Gate
	Resamples                         int
	Seed                              uint64
	PerStratum                        int
	Now                               time.Time
	Notes                             []string
}

// BuildReport 算 Kappa 与区间、混淆矩阵、专家分层与门禁结论。
func BuildReport(in ReportInput) Report {
	if in.PerStratum == 0 {
		in.PerStratum = DefaultPerStratum
	}
	jm, em := matrixOf(in.JudgeExpert), matrixOf(in.InterExpert)
	r := Report{TaskType: in.TaskType, Dimension: in.Dimension, JudgeVersion: in.JudgeVersion, GeneratedAt: in.Now,
		Threshold: in.Gate.KappaThreshold, JudgeMatrix: jm, ExpertMatrix: em,
		JudgeExpert: BootstrapKappa(jm.Pairs(), in.Resamples, in.Seed),
		InterExpert: BootstrapKappa(em.Pairs(), in.Resamples, in.Seed),
		Enabled:     in.Gate.Dimensions[in.Dimension].Enabled, Notes: in.Notes}
	if in.UnknownOK {
		u := in.UnknownRatio
		r.UnknownRatio = &u
	}
	for i := range categories {
		for j := range categories {
			r.ExpertStrata[j] += jm[i][j]
		}
	}
	for j, n := range r.ExpertStrata {
		if n < in.PerStratum {
			r.Short = append(r.Short, fmt.Sprintf("expert=%d: %d < %d", j+minScore, n, in.PerStratum))
		}
	}
	r.GateReason = gateReason(r)
	r.GateReady = r.GateReason == ""
	return r
}

// gateReason 返回不可进门禁的原因；空串表示可进。
func gateReason(r Report) string {
	iv := r.JudgeExpert
	switch {
	case r.TaskType != store.TaskCalibration:
		return fmt.Sprintf("%s 只报一致性，不产出门禁结论", r.TaskType)
	case iv.N < MinCalibrationPairs:
		return fmt.Sprintf("judge-专家评分对 n=%d < %d", iv.N, MinCalibrationPairs)
	case !iv.OK:
		return "Kappa 无定义"
	case !iv.Usable:
		return fmt.Sprintf("bootstrap 无定义重抽 %d/%d 超过 %.0f%%，区间不可用", iv.Undefined, iv.Resamples, MaxUndefinedRatio*100)
	case iv.Low < r.Threshold:
		return fmt.Sprintf("区间下限 %.3f < 阈值 %.2f", iv.Low, r.Threshold)
	}
	return ""
}

func matrixOf(ps []store.ScorePair) Matrix {
	var m Matrix
	for _, p := range ps {
		if inRange(p.A) && inRange(p.B) {
			m[p.A-minScore][p.B-minScore] += p.Count
		}
	}
	return m
}

// Markdown 输出报告（本地 eval/out/）。
func (r Report) Markdown(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# judge %s 一致性报告（%s）\n\n", r.Dimension, r.TaskType)
	fmt.Fprintf(&b, "- judge_version: `%s`\n- generated_at: %s\n", r.JudgeVersion, r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- judge-专家加权 Kappa: %s\n", kappaLine(r.JudgeExpert))
	fmt.Fprintf(&b, "- 专家间加权 Kappa（可达上限）: %s\n", kappaLine(r.InterExpert))
	unknown := "无任务"
	if r.UnknownRatio != nil {
		unknown = fmt.Sprintf("%.1f%%", *r.UnknownRatio*100)
	}
	fmt.Fprintf(&b, "- UNKNOWN 占比（主标，不进 Kappa）: %s\n", unknown)
	if r.TaskType == store.TaskCalibration {
		verdict := "未达标（" + r.GateReason + "；门禁关闭，judge 只进报告）"
		if r.GateReady {
			verdict = "可进门禁（以 PR 打开 " + DefaultGate + " 中该维度）"
		}
		fmt.Fprintf(&b, "- 阈值 %.2f（待实测）对区间下限，且 n ≥ %d、无定义重抽 ≤ %.0f%%: %s；当前门禁 enabled=%v\n",
			r.Threshold, MinCalibrationPairs, MaxUndefinedRatio*100, verdict, r.Enabled)
	} else {
		fmt.Fprintf(&b, "- 门禁: %s（门禁结论只看校准报告）；当前门禁 enabled=%v\n", r.GateReason, r.Enabled)
	}
	if len(r.Short) > 0 {
		fmt.Fprintf(&b, "- 专家分层不足（待补抽）: %s\n", strings.Join(r.Short, "; "))
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	writeMatrix(&b, "judge（行）× 专家（列）", r.JudgeMatrix)
	writeMatrix(&b, "主标（行）× 双标（列）", r.ExpertMatrix)
	_, err := io.WriteString(w, b.String())
	return err
}

func kappaLine(iv Interval) string {
	if !iv.OK {
		return fmt.Sprintf("无定义（数据不足或只有单一分数；n=%d，无定义重抽 %d）", iv.N, iv.Undefined)
	}
	usable := ""
	if !iv.Usable {
		usable = "，区间不可用"
	}
	return fmt.Sprintf("%.3f，95%% 区间 [%.3f, %.3f]（n=%d，bootstrap %d 次，seed %d，无定义重抽 %d%s）",
		iv.Kappa, iv.Low, iv.High, iv.N, iv.Resamples, iv.Seed, iv.Undefined, usable)
}

func writeMatrix(b *strings.Builder, title string, m Matrix) {
	fmt.Fprintf(b, "\n## 混淆矩阵：%s，n=%d\n\n|   | 1 | 2 | 3 |\n|---|---|---|---|\n", title, m.Total())
	for i, row := range m {
		cells := make([]string, 0, categories)
		for _, c := range row {
			cells = append(cells, fmt.Sprint(c))
		}
		fmt.Fprintf(b, "| %d | %s |\n", i+minScore, strings.Join(cells, " | "))
	}
}
