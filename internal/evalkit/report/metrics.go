package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strconv"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/model"
)

// P2 L3：夜间全量的可信度指标（口径表 docs/p2/可信度指标口径表.md）写 MinIO {experiment_id}/metrics.json，
// 并更新 nightly/latest.json 指向它；有 grader_error 的实验不写（日报离线栏显示「本期作废」）。
const (
	// MetricsFile 是指标文件名：agri-steward-eval/{experiment_id}/metrics.json。
	MetricsFile = model.NightlyMetricsFile
	// LatestNightlyKey 是最近一次写出 metrics.json 的夜间全量指针（rollup 导入离线栏用）。
	LatestNightlyKey = model.LatestNightlyKey
	// MaxFailingSamples 是每个指标切片在 metrics.json 里列出的未达标样本上限（只为下钻，不参与计算）。
	MaxFailingSamples = 20
	jsonContentType   = "application/json"
)

// ErrGraderErrorTrials 表示实验有 grader_error 的 trial：当次不出数，不写 metrics.json。
var ErrGraderErrorTrials = errors.New("report: experiment has grader_error trials, metrics.json not written")

// MetricsKey 是实验 metrics.json 的对象键。
func MetricsKey(expID string) string { return path.Join(expID, MetricsFile) }

// SampleAttrs 是样本的切片维度（cmd/eval 从样本与分类表读出）：一级分类、难度、陷阱组 tag。
type SampleAttrs struct {
	Category   string
	Difficulty string
	Traps      []string
}

// MetricsInput 是 BuildMetrics 的输入。Exp 须是未脱敏的实验（holdout 脱敏会抹掉 Evidence，改变归类）。
type MetricsInput struct {
	Exp *evalkit.Experiment
	// Attrs 按 sample_id 给切片维度；缺失的样本只进 all 切片。
	Attrs map[string]SampleAttrs
	// Calibrated 报告 judge 指标是否已过 L2 校准（judge 门禁已开）；未校准的只出报告、标「未校准」。
	Calibrated bool
	Seed       uint64
	Resamples  int
	// VoidReason 非空表示门禁终态不是 DONE（VOID / BUDGET_EXCEEDED）：本期作废，不写 metrics.json。
	VoidReason string
}

// VoidGraderError 是有 grader_error trial 时 latest.json 的 void_reason。
const VoidGraderError = "GRADER_ERROR"

// MetricsStore 是 UploadMetrics 需要的对象存储：写 metrics.json / latest.json，读 latest.json 防回指。
// GetObject 在对象不存在时须返回 model.ErrNotFound（repository.QualityObjects 实现）。
type MetricsStore interface {
	ObjectPutter
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
}

// MetricsUpload 是 UploadMetrics 的结果：Key 为写出的 metrics.json（作废时为空）；VoidReason 非空表示本期作废；
// LatestMoved 为 false 表示桶里已有更新的实验，latest.json 未改。
type MetricsUpload struct {
	Key         string
	VoidReason  string
	LatestMoved bool
}

// BuildMetrics 按切片（all、category=、difficulty=、trap=）汇总六个可信度指标与引用正确率。有 grader_error 时返回
// ErrGraderErrorTrials。
func BuildMetrics(in MetricsInput) (model.OfflineMetrics, error) {
	id, err := strconv.ParseUint(in.Exp.ID, 10, 64)
	if err != nil {
		return model.OfflineMetrics{}, fmt.Errorf("report: metrics experiment id %q: %w", in.Exp.ID, err)
	}
	if slices.ContainsFunc(in.Exp.Trials, func(t evalkit.Trial) bool { return t.Status() == evalkit.StatusGraderError }) {
		return model.OfflineMetrics{}, fmt.Errorf("%w: exp %s", ErrGraderErrorTrials, in.Exp.ID)
	}
	out := model.OfflineMetrics{ExperimentID: id, FinishedAt: in.Exp.FinishedAt.UTC(), Seed: in.Seed, Resamples: in.Resamples,
		Metrics: []model.OfflineMetric{}}
	groups := sliceTrials(in.Exp.Trials, in.Attrs)
	for _, m := range slices.Concat(reduce.QualityMetrics, reduce.ReportOnlyMetrics) {
		for _, s := range groups {
			out.Metrics = append(out.Metrics, offlineMetric(m, s, in))
		}
	}
	return out, nil
}

type trialSlice struct {
	key    string
	trials []evalkit.Trial
}

// sliceTrials 按切片键分组（all 在前，其余按键排序）。
func sliceTrials(trials []evalkit.Trial, attrs map[string]SampleAttrs) []trialSlice {
	groups := map[string][]evalkit.Trial{}
	for _, t := range trials {
		for _, k := range sliceKeys(attrs[t.SampleID]) {
			groups[k] = append(groups[k], t)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		if k != model.SliceAll {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := []trialSlice{{key: model.SliceAll, trials: trials}}
	for _, k := range keys {
		out = append(out, trialSlice{key: k, trials: groups[k]})
	}
	return out
}

// sliceKeys 是一个样本所属的切片：all 外加非空的 category / difficulty 与每个陷阱组。
func sliceKeys(a SampleAttrs) []string {
	keys := []string{model.SliceAll}
	if a.Category != "" {
		keys = append(keys, "category="+a.Category)
	}
	if a.Difficulty != "" {
		keys = append(keys, "difficulty="+a.Difficulty)
	}
	for _, t := range a.Traps {
		keys = append(keys, "trap="+t)
	}
	return keys
}

func offlineMetric(m reduce.QualityMetric, s trialSlice, in MetricsInput) model.OfflineMetric {
	sum := reduce.Summarize(m, s.trials, in.Seed, in.Resamples)
	om := model.OfflineMetric{Metric: m.Name(), Title: m.Title(), Slice: s.key, Status: string(sum.Status),
		RedLine: m.RedLine(), Judge: m.Judge(), Calibrated: !m.Judge() || in.Calibrated, LowerIsBetter: m.LowerIsBetter(),
		Numerator: sum.Numerator, Denominator: sum.Denominator, InfraErrors: sum.InfraErrors, GraderErrors: sum.GraderErrors,
		Unknown: sum.Unknown, UnknownShare: sum.UnknownShare, Samples: sum.Samples, Point: sum.Point, Low: sum.Low, High: sum.High,
		Interval: string(sum.Interval)}
	if sum.Status == reduce.StatusOK || sum.Status == reduce.StatusUndeterminable {
		om.Failing = failingSamples(m, s.trials)
	}
	return om
}

// failingSamples 是样本级值未达最好值（越高越好为 1，越低越好为 0）的 sample_id，按键排序、截断到 MaxFailingSamples。
func failingSamples(m reduce.QualityMetric, trials []evalkit.Trial) []string {
	best := 1.0
	if m.LowerIsBetter() {
		best = 0
	}
	var out []string
	for _, v := range reduce.SampleValues(m, trials) {
		if v.Value != best {
			out = append(out, v.Key)
		}
		if len(out) == MaxFailingSamples {
			break
		}
	}
	return out
}

// UploadMetrics 构造并上传 metrics.json，再更新 nightly/latest.json。in.VoidReason 非空（门禁终态非 DONE）或有
// grader_error 时不写 metrics.json，只把 nightly/latest.json 置为本期作废（void）：日报据此显示「本期作废」，不沿用
// 上一期的数。指针只前进：桶里已有更新的实验（LatestNightly.Newer）时不改 latest.json（补跑旧实验不回指）。
func UploadMetrics(ctx context.Context, obj MetricsStore, bucket string, in MetricsInput) (MetricsUpload, error) {
	id, err := strconv.ParseUint(in.Exp.ID, 10, 64)
	if err != nil {
		return MetricsUpload{}, fmt.Errorf("report: metrics experiment id %q: %w", in.Exp.ID, err)
	}
	latest := model.LatestNightly{ExperimentID: id, StartedAt: in.Exp.StartedAt.UTC(), FinishedAt: in.Exp.FinishedAt.UTC(),
		Void: in.VoidReason != "", VoidReason: in.VoidReason}
	var om model.OfflineMetrics
	if !latest.Void {
		om, err = BuildMetrics(in)
		switch {
		case errors.Is(err, ErrGraderErrorTrials):
			latest.Void, latest.VoidReason = true, VoidGraderError
		case err != nil:
			return MetricsUpload{}, err
		}
	}
	var out MetricsUpload
	if !latest.Void {
		body, err := json.MarshalIndent(om, "", "  ")
		if err != nil {
			return MetricsUpload{}, fmt.Errorf("report: marshal metrics: %w", err)
		}
		key := MetricsKey(in.Exp.ID)
		if err := obj.PutObject(ctx, bucket, key, body, jsonContentType); err != nil {
			return MetricsUpload{}, fmt.Errorf("report: upload %s: %w", key, err)
		}
		out.Key = key
	}
	out.VoidReason = latest.VoidReason
	if out.LatestMoved, err = putLatest(ctx, obj, bucket, latest); err != nil {
		return MetricsUpload{}, err
	}
	return out, nil
}

// putLatest 在桶里没有更新的指针时写 latest.json，返回是否写了。读指针失败（对象不存在除外）或解码失败即报错，不盲写。
// 读与写之间不加锁：夜间全量每日一次，并发写指针的窗口只在两个 nightly 同时收尾时出现。
func putLatest(ctx context.Context, obj MetricsStore, bucket string, latest model.LatestNightly) (bool, error) {
	cur, err := obj.GetObject(ctx, bucket, LatestNightlyKey)
	switch {
	case errors.Is(err, model.ErrNotFound):
	case err != nil:
		return false, fmt.Errorf("report: read %s: %w", LatestNightlyKey, err)
	default:
		var prev model.LatestNightly
		if err := json.Unmarshal(cur, &prev); err != nil {
			return false, fmt.Errorf("report: decode %s: %w", LatestNightlyKey, err)
		}
		if prev.Newer(latest) {
			return false, nil
		}
	}
	body, err := json.Marshal(latest)
	if err != nil {
		return false, fmt.Errorf("report: marshal latest nightly: %w", err)
	}
	if err := obj.PutObject(ctx, bucket, LatestNightlyKey, body, jsonContentType); err != nil {
		return false, fmt.Errorf("report: upload %s: %w", LatestNightlyKey, err)
	}
	return true, nil
}
