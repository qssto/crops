package judge

import (
	"context"
	"fmt"

	"agri-steward/internal/evalkit/grader"
	qjudge "agri-steward/internal/service/quality/judge"
)

// 分数范围（1-3 有序量表）。
const (
	MinScore = qjudge.MinScore
	MaxScore = qjudge.MaxScore
)

var (
	// ErrEvidenceNotQuoted 表示 evidence 不是被评文本原句（Fold 后非子串）。
	ErrEvidenceNotQuoted = qjudge.ErrEvidenceNotQuoted
	// ErrBadScore 表示 judge 给出的分数不在 1-3。
	ErrBadScore = qjudge.ErrBadScore
	// ErrEvidenceTooShort 表示 evidence 归一后不足 MinEvidenceRunes 个字。
	ErrEvidenceTooShort = qjudge.ErrEvidenceTooShort
	// ErrEvidenceCrossSentence 表示 evidence 中间有句末标点或换行，是拼接的多句而非一句原句。
	ErrEvidenceCrossSentence = qjudge.ErrEvidenceCrossSentence
)

// MinEvidenceRunes 是 evidence 归一后的最少字数（L2 评审 P2-6）。
const MinEvidenceRunes = qjudge.MinEvidenceRunes

// Judge 是一个维度的 LLM-judge 打分器。judge 在门禁配置里默认关闭、只进报告，故 Pass 恒为 true（不改变 trial 状态），
// 分数放 Value（1-3），UNKNOWN 记 Score.Unknown（转人工队列）。evidence 不合格或分数越界：维度门禁开（gated）时
// 记 grader_error，门禁关时 Advisory，只记 Score.AdvisoryError、不改 trial 状态（L2 评审裁决）。
type Judge struct {
	prompt Prompt
	client Client
	gated  bool
}

// New 读 dir/<dim>/{prompt.md,rubric.md} 构造 dim 维度的 judge。
func New(dir, dim string, c Client) (*Judge, error) {
	p, err := qjudge.LoadPrompt(dir, dim)
	if err != nil {
		return nil, err
	}
	return &Judge{prompt: p, client: c}, nil
}

// All 构造三个维度的 judge（共用一个 Client）；gated(dim) 为真的维度已进门禁、出错记 grader_error，
// gated 为 nil 视为全部未进门禁。
func All(dir string, c Client, gated func(dim string) bool) ([]grader.Grader, error) {
	out := make([]grader.Grader, 0, len(Dimensions))
	for _, d := range Dimensions {
		j, err := New(dir, d, c)
		if err != nil {
			return nil, err
		}
		j.gated = gated != nil && gated(d)
		out = append(out, j)
	}
	return out, nil
}

func (j *Judge) Name() string  { return j.prompt.Dimension }
func (j *Judge) RedLine() bool { return false }

// Advisory 实现 grader.Advisory：门禁未开的维度出错不改 trial 状态。
func (j *Judge) Advisory() bool { return !j.gated }

// Grade 组装材料、调 judge 模型并校验结果。
func (j *Judge) Grade(ctx context.Context, t *grader.Trial) (grader.Score, error) {
	m, err := BuildMaterial(ctx, j.prompt.Dimension, t)
	if err != nil {
		return grader.Score{}, err
	}
	v, err := j.client.Judge(ctx, j.prompt, m)
	if err != nil {
		return grader.Score{}, fmt.Errorf("judge %s: %w", j.prompt.Dimension, err)
	}
	return ToScore(v, m.EvaluatedText())
}

// ToScore 把 judge 结果转为打分：UNKNOWN → Unknown；分数越界、evidence 不是 evaluated 的一句原句（见 qjudge.Validate）→ 错误。
func ToScore(v Verdict, evaluated string) (grader.Score, error) {
	if v.Unknown() {
		return grader.Score{Pass: true, Unknown: true, Evidence: "UNKNOWN: " + v.Verdict}, nil
	}
	if err := qjudge.Validate(v, evaluated); err != nil {
		return grader.Score{}, err
	}
	return grader.Score{Pass: true, Value: float64(*v.Score),
		Evidence: fmt.Sprintf("score=%d %s｜%s", *v.Score, v.Verdict, v.Evidence)}, nil
}
