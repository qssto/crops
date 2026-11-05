package quality

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agri-steward/internal/model"
)

// 离线栏：读 eval 桶的 nightly/latest.json 与 {experiment_id}/metrics.json（评测侧写，本包只读，不 import evalkit）。

// OfflineState 是日报离线栏的状态。
type OfflineState string

// 离线栏状态。
const (
	OfflineOK     OfflineState = "OK"
	OfflineVoid   OfflineState = "VOID"    // 最近一次夜间全量有 grader_error：「本期作废」
	OfflineNoData OfflineState = "NO_DATA" // 还没有夜间全量：「无数据」
)

// Offline 是最近一次夜间全量（State=OK 时 Metrics 非 nil）。
type Offline struct {
	State   OfflineState
	Latest  model.LatestNightly
	Metrics *model.OfflineMetrics
}

// LoadOffline 读最近一次夜间全量。指针或 metrics.json 不存在记 NO_DATA，指针 void 记 VOID。
func LoadOffline(ctx context.Context, obj Objects, bucket string) (Offline, error) {
	body, err := obj.GetObject(ctx, bucket, model.LatestNightlyKey)
	if errors.Is(err, model.ErrNotFound) {
		return Offline{State: OfflineNoData}, nil
	}
	if err != nil {
		return Offline{}, err
	}
	var latest model.LatestNightly
	if err := json.Unmarshal(body, &latest); err != nil {
		return Offline{}, fmt.Errorf("quality: decode %s: %w", model.LatestNightlyKey, err)
	}
	if latest.Void {
		return Offline{State: OfflineVoid, Latest: latest}, nil
	}
	m, err := LoadMetrics(ctx, obj, bucket, latest.ExperimentID)
	if errors.Is(err, model.ErrNotFound) {
		return Offline{State: OfflineNoData, Latest: latest}, nil
	}
	if err != nil {
		return Offline{}, err
	}
	return Offline{State: OfflineOK, Latest: latest, Metrics: m}, nil
}

// LoadMetrics 读一个实验的 metrics.json（基线 baseline_p1.r2 也用它）；不存在返回 model.ErrNotFound。
func LoadMetrics(ctx context.Context, obj Objects, bucket string, expID uint64) (*model.OfflineMetrics, error) {
	key := model.NightlyMetricsKey(expID)
	body, err := obj.GetObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	var m model.OfflineMetrics
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("quality: decode %s: %w", key, err)
	}
	return &m, nil
}

// OfflineRows 是导入 quality_metric_daily 的离线平台行（tenant_id=0, farm_id=0, source=OFFLINE）：只导 Status=OK 的
// 指标切片，带 experiment_id；judge 维度 UNKNOWN 已从分子分母剔除，区间按样本计（与 metrics.json 一致）。
func OfflineRows(date string, m *model.OfflineMetrics) []model.QualityDaily {
	if m == nil {
		return nil
	}
	var out []model.QualityDaily
	for _, x := range m.Metrics {
		if x.Status != model.MetricStatusOK {
			continue
		}
		out = append(out, model.QualityDaily{StatDate: date, Source: model.MetricSourceOffline, Metric: x.Metric,
			SliceKey: x.Slice, Numerator: x.Numerator, Denominator: x.Denominator, Point: round5(x.Point),
			Low: ptr(round5(x.Low)), High: ptr(round5(x.High)), IsEstimate: false, ExperimentID: m.ExperimentID})
	}
	return out
}
