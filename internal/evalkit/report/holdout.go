package report

import (
	"context"
	"fmt"
	"path"
	"strings"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
)

// HoldoutPrefix 是 holdout trial 的 transcript 与报告明细在 eval bucket 内的前缀（P2 §6.3 技术实现 5，仅负责人可读）。
const HoldoutPrefix = "report/holdout"

// RedactedText 替换公开报告里 holdout trial 的证据与错误文本（保留非空，使 trial 状态不变）。
const RedactedText = "(holdout: see " + HoldoutPrefix + "/)"

// IsHoldoutTrial 报告 trial 是否来自 holdout（按加载时的路径标，见 evalkit.Trial.IsHoldout）。
func IsHoldoutTrial(t evalkit.Trial) bool { return t.IsHoldout() }

// HoldoutReportKey 是 holdout 明细报告的对象键：report/holdout/{exp_id}/report.md。
func HoldoutReportKey(expID string) string { return path.Join(HoldoutPrefix, expID, FileName) }

// RedactHoldout 返回供公开报告用的副本：holdout trial 只留 ID、trial 序号、run_id、期望 / 实际 outcome、状态与打分器名，
// 去掉证据、错误文本、tag、拒答原因、未声明调用与链路明细；holdout 隔离样本只留 ID（套件记 holdout，理由换成占位，
// L2 评审 P2-7）。不修改 exp。
func RedactHoldout(exp *evalkit.Experiment) *evalkit.Experiment {
	out := *exp
	out.Trials = make([]evalkit.Trial, len(exp.Trials))
	for i, t := range exp.Trials {
		if IsHoldoutTrial(t) {
			t = redactTrial(t)
		}
		out.Trials[i] = t
	}
	if exp.Quarantined != nil {
		out.Quarantined = make([]evalkit.QuarantinedSample, len(exp.Quarantined))
		for i, q := range exp.Quarantined {
			if q.IsHoldout() {
				q = evalkit.QuarantinedSample{ID: q.ID, Suite: sample.HoldoutDir, Reason: RedactedText, Holdout: true}
			}
			out.Quarantined[i] = q
		}
	}
	return &out
}

func redactTrial(t evalkit.Trial) evalkit.Trial {
	r := evalkit.Trial{SampleID: t.SampleID, SampleVersion: t.SampleVersion, Suite: t.Suite, TrialIndex: t.TrialIndex,
		Expected: t.Expected, Outcome: t.Outcome, RunID: t.RunID, Latency: t.Latency, Transcript: t.Transcript}
	if t.Error != "" {
		r.Error = RedactedText
	}
	r.Scores = make([]evalkit.Score, len(t.Scores))
	for i, sc := range t.Scores {
		sc.Evidence = ""
		if !sc.Pass || sc.Error != "" {
			sc.Evidence = RedactedText
		}
		if sc.Error != "" {
			sc.Error = RedactedText
		}
		if sc.AdvisoryError != "" {
			sc.AdvisoryError = RedactedText
		}
		r.Scores[i] = sc
	}
	return r
}

// UploadHoldoutReport 实验含 holdout trial 时，把只含 holdout trial 的完整明细报告上传 HoldoutReportKey，返回对象键；
// 不含 holdout trial 时不上传、返回空串。
func UploadHoldoutReport(ctx context.Context, obj ObjectPutter, bucket string, exp *evalkit.Experiment) (string, error) {
	ho := *exp
	ho.Trials = nil
	for _, t := range exp.Trials {
		if IsHoldoutTrial(t) {
			ho.Trials = append(ho.Trials, t)
		}
	}
	if len(ho.Trials) == 0 {
		return "", nil
	}
	var b strings.Builder
	if err := Render(&b, &ho); err != nil {
		return "", err
	}
	key := HoldoutReportKey(exp.ID)
	if err := obj.PutObject(ctx, bucket, key, []byte(b.String()), ReportContentType); err != nil {
		return "", fmt.Errorf("report: upload holdout report %s: %w", key, err)
	}
	return key, nil
}
