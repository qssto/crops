package model

import (
	"strconv"
	"strings"
	"time"
)

// P2 L3 可信度指标（口径表 docs/p2/可信度指标口径表.md）：夜间全量 metrics.json 与 quality_metric_daily 共用的取值。
// 生产侧（rollup、日报）只经这些类型读离线结果，不 import evalkit。

// 指标汇总状态（evalkit/reduce.Summary.Status 取同一组值）。
const (
	MetricStatusOK             = "OK"
	MetricStatusNoData         = "NO_DATA"
	MetricStatusUndeterminable = "UNDETERMINABLE" // judge UNKNOWN 占比 > 10%：不可判
	MetricStatusVoid           = "VOID"           // 有 grader_error：不出数字
)

// quality_metric_daily.source 与切片键。
const (
	MetricSourceOnline  = "ONLINE"
	MetricSourceOffline = "OFFLINE"
	// SliceAll 是门禁唯一看的切片；其余切片键形如 category=protect、difficulty=hard、trap=trap.overdose。
	SliceAll = "all"
)

// OfflineMetrics 是夜间全量写 MinIO {experiment_id}/metrics.json 的内容；有 grader_error 的实验不写（日报显示「本期作废」）。
type OfflineMetrics struct {
	ExperimentID uint64          `json:"experiment_id"`
	FinishedAt   time.Time       `json:"finished_at"`
	Seed         uint64          `json:"seed"`
	Resamples    int             `json:"resamples"`
	Metrics      []OfflineMetric `json:"metrics"`
}

// OfflineMetric 是一个指标在一个切片上的汇总。Numerator / Denominator 是 trial 级计数，Point / Low / High 是样本级估计。
// Failing 是样本级值未达最好值的 sample_id@version（按键排序、截断），日报据此下钻到 report.html#sample-{id}。
type OfflineMetric struct {
	Metric        string   `json:"metric"`
	Title         string   `json:"title"`
	Slice         string   `json:"slice"`
	Status        string   `json:"status"`
	RedLine       bool     `json:"red_line"`
	Judge         bool     `json:"judge"`
	Calibrated    bool     `json:"calibrated"`
	LowerIsBetter bool     `json:"lower_is_better"`
	Numerator     int      `json:"numerator"`
	Denominator   int      `json:"denominator"`
	InfraErrors   int      `json:"infra_errors"`
	GraderErrors  int      `json:"grader_errors"`
	Unknown       int      `json:"unknown"`
	UnknownShare  float64  `json:"unknown_share"`
	Samples       int      `json:"samples"`
	Point         float64  `json:"point"`
	Low           float64  `json:"ci_low"`
	High          float64  `json:"ci_high"`
	Interval      string   `json:"interval"`
	Failing       []string `json:"failing,omitempty"`
}

// LatestNightly 是 MinIO nightly/latest.json：最近一次夜间全量（rollup 据此导入离线栏）。Void 为真表示该次没写
// metrics.json（门禁终态非 DONE，或有 grader_error），日报离线栏显示「本期作废」，VoidReason 记原因。
// 「最近」按 StartedAt 定（同刻按 ExperimentID）：resume 会把 finished_at 改成补跑时刻，不能用它排序。
type LatestNightly struct {
	ExperimentID uint64    `json:"experiment_id"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Void         bool      `json:"void,omitempty"`
	VoidReason   string    `json:"void_reason,omitempty"`
}

// Newer 报告 l 是否比 other 更新（StartedAt 晚，或同刻 ExperimentID 更大）；旧的指针不得覆盖新的。
func (l LatestNightly) Newer(other LatestNightly) bool {
	if !l.StartedAt.Equal(other.StartedAt) {
		return l.StartedAt.After(other.StartedAt)
	}
	return l.ExperimentID > other.ExperimentID
}

// eval 桶（agri-steward-eval）里 L3 的对象键：评测侧写、rollup / 日报读，两边共用同一组键。
const (
	// NightlyMetricsFile 是实验指标文件名：{experiment_id}/metrics.json。
	NightlyMetricsFile = "metrics.json"
	// LatestNightlyKey 是最近一次夜间全量的指针。
	LatestNightlyKey = "nightly/latest.json"
	// IntervalWilson 是 OfflineMetric.Interval 的 Wilson 取值（其余为 bootstrap）。
	IntervalWilson = "wilson"
)

// NightlyMetricsKey 是实验 metrics.json 的对象键。
func NightlyMetricsKey(expID uint64) string {
	return strconv.FormatUint(expID, 10) + "/" + NightlyMetricsFile
}

// NightlyReportPath 是实验下钻页相对日报（report/daily/）的路径。
func NightlyReportPath(expID uint64) string {
	return "../../" + strconv.FormatUint(expID, 10) + "/report.html"
}

// DailyReportKey 是日报对象键 report/daily/{yyyymmdd}.html；date 为 yyyy-mm-dd。
func DailyReportKey(date string) string {
	return "report/daily/" + strings.ReplaceAll(date, "-", "") + ".html"
}

// DailyNotifiedKey 是日报已推送标记 report/daily/{yyyymmdd}.sent.json：存在即该日已完成，多实例 / 重启都不再重跑、不重复推送。
func DailyNotifiedKey(date string) string {
	return "report/daily/" + strings.ReplaceAll(date, "-", "") + ".sent.json"
}

// DailyNotified 是 DailyNotifiedKey 的内容。
type DailyNotified struct {
	Date      string    `json:"date"`
	ReportKey string    `json:"report_key"`
	SentAt    time.Time `json:"sent_at"`
}

// SampleAnchor 是下钻页 {experiment_id}/report.html 里样本的锚点 sample-{id}；key 可带 @version（取最后一个 @ 之前）。
func SampleAnchor(key string) string {
	if i := strings.LastIndex(key, "@"); i > 0 {
		key = key[:i]
	}
	return "sample-" + key
}

// 线上指标名（口径表「线上口径」）：引用覆盖率 / 数值准确率 / 事实一致率与离线同名，拒答率只有线上值。
const (
	MetricCitationCoverage = "citation_coverage"
	MetricNumericAccuracy  = "numeric_accuracy"
	MetricFactConsistency  = "fact_consistency"
	MetricRefusalRate      = "refusal_rate"
)

// QualityDaily 是 quality_metric_daily 的一行。tenant_id=0, farm_id=0 为平台汇总行；StatDate 是农场时区下的业务日
// （yyyy-mm-dd）；Low / High 为 nil 表示无区间；ExperimentID 只在 OFFLINE 行非 0。
type QualityDaily struct {
	TenantID     uint64
	FarmID       uint64
	StatDate     string
	Source       string
	Metric       string
	SliceKey     string
	Numerator    int
	Denominator  int
	Point        float64
	Low          *float64
	High         *float64
	IsEstimate   bool
	ExperimentID uint64
}

// online_judge_result.status（P2 §6.4 技术实现 3）。
const (
	JudgeStatusScored     = "SCORED"
	JudgeStatusNeedExpert = "NEED_EXPERT" // judge UNKNOWN 或 1 分：推专家巡检
	JudgeStatusReviewed   = "REVIEWED"    // 专家已回写
)

// OnlineJudgeResult 是 online_judge_result 的一行；JudgeScore / ExpertScore 为 nil 表示 UNKNOWN / 未回写。
type OnlineJudgeResult struct {
	ID           uint64
	TenantID     uint64
	FarmID       uint64
	RunID        uint64
	AdviceID     uint64
	StatDate     string
	Category     string
	Dimension    string
	JudgeVersion string
	JudgeScore   *int
	ExpertScore  *int
	ExpertID     uint64
	Status       string
	RowVer       int
}

// Score 是该行计入事实一致率的分数：专家已回写取专家分，否则取 judge 分；nil 为不可判（UNKNOWN）。
func (r OnlineJudgeResult) Score() *int {
	if r.Status == JudgeStatusReviewed && r.ExpertScore != nil {
		return r.ExpertScore
	}
	return r.JudgeScore
}
