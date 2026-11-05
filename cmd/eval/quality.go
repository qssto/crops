package main

import (
	"context"
	"fmt"
	"io"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/reduce"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
)

// nightlyQuality（P2 L3，路线 §6.4 技术实现 ① 1）：只在 trigger=nightly 时，夜间全量结束后写 MinIO
// {experiment_id}/metrics.json、nightly/latest.json 与下钻页 {experiment_id}/report.html。其他触发方式不调用，现有报告不变。
// 门禁终态不是 DONE（VOID / BUDGET_EXCEEDED / 未判定）或有 grader_error 时本期作废：不写 metrics.json，latest.json 记
// void（日报离线栏「本期作废」）。选「按 void 写」而不是「不更新 latest」：后者会让日报继续展示上一期的数，掩盖当晚作废。
// latest.json 只前进（按 started_at），resume 补跑旧实验不回指。
//
// full 是未脱敏的实验（指标按 Evidence 归类，holdout 脱敏会改变归类）；pub 是公开副本（下钻页只取 ID 与状态）。
func nightlyQuality(ctx context.Context, obj report.MetricsStore, bucket, trigger string, ls *loadedSamples, jf *judgeFlags,
	full, pub *evalkit.Experiment, stdout io.Writer) error {
	if trigger != store.TriggerNightly {
		return nil
	}
	calibrated, err := jf.factCalibrated()
	if err != nil {
		return err
	}
	drill, err := report.UploadDrilldown(ctx, obj, bucket, pub)
	if err != nil {
		return err
	}
	up, err := report.UploadMetrics(ctx, obj, bucket, report.MetricsInput{Exp: full, Attrs: sampleAttrs(ls),
		Calibrated: calibrated, Seed: reduce.DefaultSeed, Resamples: reduce.DefaultResamples, VoidReason: gateVoidReason(full.Gate)})
	if err != nil {
		return err
	}
	if up.VoidReason != "" {
		fmt.Fprintf(stdout, "quality metrics: exp %s void (%s), metrics.json not written (本期作废); drilldown=%s/%s latest_moved=%t\n",
			full.ID, up.VoidReason, bucket, drill, up.LatestMoved)
		return nil
	}
	fmt.Fprintf(stdout, "quality metrics: %s/%s drilldown=%s/%s latest_moved=%t\n", bucket, up.Key, bucket, drill, up.LatestMoved)
	return nil
}

// gateDone 是实验门禁通过的终态（store.settleTx 不命中作废规则时保持 DONE）。
const gateDone = "DONE"

// gateUnsettled 是门禁未判定（Gate.Status 为空）时的作废原因：失败关闭，不把未判定的实验当通过。
const gateUnsettled = "UNSETTLED"

// gateVoidReason：门禁终态为 DONE 时返回空；否则返回作废原因（VOID 带 void_reason，其余为终态名）。
func gateVoidReason(g evalkit.GateStatus) string {
	switch g.Status {
	case gateDone:
		return ""
	case "":
		return gateUnsettled
	case store.StatusVoid:
		if g.VoidReason != "" {
			return store.StatusVoid + ":" + g.VoidReason
		}
	}
	return g.Status
}

// factCalibrated 报告 fact_consistency 维度是否已过 L2 校准（judge 门禁配置里 enabled）；未开 --judges 视为未校准。
func (f *judgeFlags) factCalibrated() (bool, error) {
	if !f.enabled {
		return false, nil
	}
	gate, err := review.LoadGate(f.gate)
	if err != nil {
		return false, fmt.Errorf("quality metrics: %w", err)
	}
	return gate.Dimensions[judge.DimFactConsistency].Enabled, nil
}

// sampleAttrs 读出每个样本的切片维度：一级分类（分类表）、难度、陷阱组 tag。
func sampleAttrs(ls *loadedSamples) map[string]report.SampleAttrs {
	out := make(map[string]report.SampleAttrs, len(ls.samples))
	for _, s := range ls.samples {
		st := sample.StrataOf(s, ls.tx)
		out[s.ID] = report.SampleAttrs{Category: st.Category, Difficulty: st.Difficulty, Traps: sample.TrapTags(s)}
	}
	return out
}
