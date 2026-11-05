package judge

import (
	"context"

	qjudge "agri-steward/internal/service/quality/judge"
)

// 录制回放与线上抽检共用 service/quality/judge（P2 L3 下沉）；这里只保留离线打分器的入口与类型别名。

// Prompt 是一次 judge 调用的提示词与评分标准（eval/judges/<维度>/ 原文）。
type Prompt = qjudge.Prompt

// Verdict 是 judge 模型的解析结果。Score 为 1-3；nil 表示 UNKNOWN（信息不足或无录制）。
type Verdict = qjudge.Verdict

// Client 是 judge 模型接缝：按提示词评一份材料。返回 err 记 grader_error。
type Client = qjudge.Client

// Recording 是一条录制（eval/judges/<维度>/recordings/*.json）：按 (dimension, material_hash) 回放。
type Recording = qjudge.Recording

// ErrBadRecording 表示录制文件格式错误、维度不符、分数越界或 (维度, 材料哈希) 重复。
var ErrBadRecording = qjudge.ErrBadRecording

type recKey = qjudge.RecKey

// Replay 是录制回放客户端（不访问模型服务）：命中录制返回录制结果，没有录制返回 UNKNOWN（不报 infra_error）。
type Replay struct {
	recs qjudge.Recordings
}

var _ Client = (*Replay)(nil)

// LoadReplay 读 dir/<维度>/recordings/*.json（目录不存在视为无录制）。
func LoadReplay(dir string) (*Replay, error) {
	recs, err := qjudge.LoadRecordings(dir)
	if err != nil {
		return nil, err
	}
	return &Replay{recs: recs}, nil
}

// Add 加一条录制（dim 须与录制的 dimension 一致）。
func (r *Replay) Add(dim string, rec Recording) error { return r.recs.Add(dim, rec) }

// Judge 按 (维度, 材料哈希) 回放；无录制为 UNKNOWN。
func (r *Replay) Judge(ctx context.Context, p Prompt, m Material) (Verdict, error) {
	return r.recs.Judge(ctx, p, m)
}
