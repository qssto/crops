package judge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/textnorm"
)

// 分数范围（1-3 有序量表）。
const (
	MinScore = 1
	MaxScore = 3
)

var (
	// ErrEvidenceNotQuoted 表示 evidence 不是被评文本原句（textnorm.Fold 后非子串）。
	ErrEvidenceNotQuoted = errors.New("judge: evidence is not a sentence of the evaluated output")
	// ErrBadScore 表示 judge 给出的分数不在 1-3。
	ErrBadScore = errors.New("judge: score out of range")
	// ErrEvidenceTooShort 表示 evidence 归一后不足 MinEvidenceRunes 个字（几个字的片段几乎总能在原文里找到）。
	ErrEvidenceTooShort = errors.New("judge: evidence too short")
	// ErrEvidenceCrossSentence 表示 evidence 中间有句末标点或换行，是拼接的多句而非一句原句。
	ErrEvidenceCrossSentence = errors.New("judge: evidence spans more than one sentence")
)

// MinEvidenceRunes 是 evidence 经 textnorm.Fold 归一后的最少字数（L2 评审 P2-6）。
const MinEvidenceRunes = 8

// sentenceEnds 是断句符号（全角与半角）；evidence 只许在结尾带它们。
const sentenceEnds = "。！？；!?;\n\r"

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
	if err := CheckDim(dim); err != nil {
		return nil, err
	}
	p := Prompt{Dimension: dim}
	for _, f := range []struct {
		name string
		dst  *string
	}{{PromptFile, &p.Prompt}, {RubricFile, &p.Rubric}} {
		b, err := os.ReadFile(filepath.Join(dir, dim, f.name))
		if err != nil {
			return nil, fmt.Errorf("judge: %s: %w", dim, err)
		}
		*f.dst = string(b)
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

// ToScore 把 judge 结果转为打分：UNKNOWN → Unknown；分数越界、evidence 不是 evaluated 的一句原句（见 checkEvidence）→ 错误。
func ToScore(v Verdict, evaluated string) (grader.Score, error) {
	if v.Unknown() {
		return grader.Score{Pass: true, Unknown: true, Evidence: "UNKNOWN: " + v.Verdict}, nil
	}
	if *v.Score < MinScore || *v.Score > MaxScore {
		return grader.Score{}, fmt.Errorf("%w: %d", ErrBadScore, *v.Score)
	}
	if err := checkEvidence(v.Evidence, evaluated); err != nil {
		return grader.Score{}, err
	}
	return grader.Score{Pass: true, Value: float64(*v.Score),
		Evidence: fmt.Sprintf("score=%d %s｜%s", *v.Score, v.Verdict, v.Evidence)}, nil
}

// checkEvidence 校验 evidence 是 evaluated 的一句原句：非空且归一后是子串、不少于 MinEvidenceRunes 个字、
// 去掉结尾断句符号后中间不再有断句符号或换行。
func checkEvidence(evidence, evaluated string) error {
	ev := textnorm.Fold(evidence)
	if ev == "" || !strings.Contains(textnorm.Fold(evaluated), ev) {
		return fmt.Errorf("%w: %q", ErrEvidenceNotQuoted, evidence)
	}
	if n := utf8.RuneCountInString(ev); n < MinEvidenceRunes {
		return fmt.Errorf("%w: %d < %d runes", ErrEvidenceTooShort, n, MinEvidenceRunes)
	}
	body := strings.TrimRightFunc(evidence, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(sentenceEnds, r)
	})
	if strings.ContainsAny(body, sentenceEnds) {
		return fmt.Errorf("%w: %q", ErrEvidenceCrossSentence, evidence)
	}
	return nil
}
