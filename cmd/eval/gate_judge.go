package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/compare"
	"agri-steward/internal/evalkit/gate"
	"agri-steward/internal/evalkit/review"
	"agri-steward/internal/repository"
)

var errBadThresholds = errors.New("gate: bad thresholds.yaml")

// loadGateThresholds 读 thresholds.yaml（按 compare.Thresholds 严格解码，未知字段报错，防拼写错静默放行）的
// 指标名 → 退化阈值（null 即 nil）；指标重复或缺名即报错。
func loadGateThresholds(p string) (map[string]*float64, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("gate: read %s: %w", p, err)
	}
	var strict compare.Thresholds
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&strict); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errBadThresholds, p, err)
	}
	out := make(map[string]*float64, len(strict.Metrics))
	for _, m := range strict.Metrics {
		if m.Name == "" {
			return nil, fmt.Errorf("%w: %s: metric without name", errBadThresholds, p)
		}
		if _, dup := out[m.Name]; dup {
			return nil, fmt.Errorf("%w: %s: %s listed twice", errBadThresholds, p, m.Name)
		}
		out[m.Name] = m.Threshold
	}
	return out, nil
}

// judgeStatus 求候选实验每个 judge 维度的门禁状态：judges.yaml 已开、候选快照记有该维度 judge 版本、
// 且 MinIO 校准报告 report/calibration/{dim}/{version}.json 的 gate_ready 为 true 才算达标；其余为未评并写明原因
// （待真实模型 / 待专家）。读报告失败也按未评（失败关闭到 NEED_SIGNOFF，不放行）。
func judgeStatus(ctx context.Context, objs repository.ObjectGetter, bucket string, cfg *review.Gate,
	snap evalkit.Snapshot) map[string]gate.JudgeDim {
	out := map[string]gate.JudgeDim{}
	for dim, item := range cfg.Dimensions {
		out[dim] = oneJudgeStatus(ctx, objs, bucket, dim, item.Enabled, snap.JudgeVersions[dim])
	}
	return out
}

func oneJudgeStatus(ctx context.Context, objs repository.ObjectGetter, bucket, dim string, enabled bool, version string) gate.JudgeDim {
	const pending = "（待真实模型 / 待专家）"
	switch {
	case !enabled:
		return gate.JudgeDim{Why: "未评：" + review.DefaultGate + " 未打开（judges.yaml enabled=false）" + pending}
	case version == "":
		return gate.JudgeDim{Why: "未评：候选快照没有该维度 judge 版本（run --judges）" + pending}
	}
	key := path.Join(calibrationKeyPrefix, dim, version+".json")
	body, err := objs.GetObject(ctx, bucket, key)
	if err != nil {
		return gate.JudgeDim{Why: fmt.Sprintf("未评：读不到校准报告 %s（%v）%s", key, err, pending)}
	}
	var r review.Report
	if err := json.Unmarshal(body, &r); err != nil {
		return gate.JudgeDim{Why: fmt.Sprintf("未评：校准报告 %s 解析失败（%v）", key, err)}
	}
	if !r.GateReady {
		return gate.JudgeDim{Why: "未评：校准未达标（" + r.GateReason + "）" + pending}
	}
	return gate.JudgeDim{Rated: true, Why: "校准达标：" + key}
}
