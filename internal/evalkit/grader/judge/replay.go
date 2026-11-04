package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Prompt 是一次 judge 调用的提示词与评分标准（eval/judges/<维度>/ 原文）。
type Prompt struct {
	Dimension string
	Prompt    string
	Rubric    string
}

// Verdict 是 judge 模型的解析结果。Score 为 1-3；nil 表示 UNKNOWN（信息不足或无录制）。
type Verdict struct {
	Score    *int   `json:"score"`
	Evidence string `json:"evidence"`
	Verdict  string `json:"verdict"`
}

// Unknown 报告是否 UNKNOWN。
func (v Verdict) Unknown() bool { return v.Score == nil }

// Client 是 judge 模型接缝：按提示词评一份材料。返回 err 记 grader_error。
type Client interface {
	Judge(ctx context.Context, p Prompt, m Material) (Verdict, error)
}

// Recording 是一条录制（eval/judges/<维度>/recordings/*.json）：按 (dimension, material_hash) 回放。
type Recording struct {
	Dimension    string `json:"dimension"`
	MaterialHash string `json:"material_hash"`
	Verdict
}

// ErrBadRecording 表示录制文件格式错误、维度不符、分数越界或 (维度, 材料哈希) 重复。
var ErrBadRecording = errors.New("judge: bad recording")

type recKey struct{ dim, hash string }

// Replay 是录制回放客户端（不访问模型服务）：命中录制返回录制结果，没有录制返回 UNKNOWN（不报 infra_error）。
type Replay struct {
	recs map[recKey]Verdict
}

// LoadReplay 读 dir/<维度>/recordings/*.json（目录不存在视为无录制）。
func LoadReplay(dir string) (*Replay, error) {
	r := &Replay{recs: map[recKey]Verdict{}}
	for _, dim := range Dimensions {
		files, err := filepath.Glob(filepath.Join(dir, dim, RecordingsDir, "*.json"))
		if err != nil {
			return nil, fmt.Errorf("judge: glob recordings: %w", err)
		}
		for _, f := range files {
			if err := r.addFile(dim, f); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

func (r *Replay) addFile(dim, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("judge: read %s: %w", path, err)
	}
	var rec Recording
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrBadRecording, path, err)
	}
	return r.Add(dim, rec)
}

// Add 加一条录制（dim 须与录制的 dimension 一致）。
func (r *Replay) Add(dim string, rec Recording) error {
	switch {
	case rec.Dimension != dim:
		return fmt.Errorf("%w: dimension %q under %s/", ErrBadRecording, rec.Dimension, dim)
	case rec.MaterialHash == "":
		return fmt.Errorf("%w: empty material_hash", ErrBadRecording)
	case rec.Score != nil && (*rec.Score < MinScore || *rec.Score > MaxScore):
		return fmt.Errorf("%w: score %d out of %d-%d", ErrBadRecording, *rec.Score, MinScore, MaxScore)
	}
	k := recKey{dim, rec.MaterialHash}
	if _, dup := r.recs[k]; dup {
		return fmt.Errorf("%w: duplicate %s/%s", ErrBadRecording, dim, rec.MaterialHash)
	}
	r.recs[k] = rec.Verdict
	return nil
}

// Judge 按 (维度, 材料哈希) 回放；无录制为 UNKNOWN。
func (r *Replay) Judge(_ context.Context, p Prompt, m Material) (Verdict, error) {
	h, err := m.Hash()
	if err != nil {
		return Verdict{}, err
	}
	v, ok := r.recs[recKey{p.Dimension, h}]
	if !ok {
		return Verdict{Verdict: "no recording (replay)"}, nil
	}
	return v, nil
}
