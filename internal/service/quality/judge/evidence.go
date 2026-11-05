package judge

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// 分数范围（1-3 有序量表）。
const (
	MinScore = 1
	MaxScore = 3
)

var (
	// ErrEvidenceNotQuoted 表示 evidence 不是被评文本原句（Fold 后非子串）。
	ErrEvidenceNotQuoted = errors.New("judge: evidence is not a sentence of the evaluated output")
	// ErrBadScore 表示 judge 给出的分数不在 1-3。
	ErrBadScore = errors.New("judge: score out of range")
	// ErrEvidenceTooShort 表示 evidence 归一后不足 MinEvidenceRunes 个字（几个字的片段几乎总能在原文里找到）。
	ErrEvidenceTooShort = errors.New("judge: evidence too short")
	// ErrEvidenceCrossSentence 表示 evidence 中间有句末标点或换行，是拼接的多句而非一句原句。
	ErrEvidenceCrossSentence = errors.New("judge: evidence spans more than one sentence")
)

// MinEvidenceRunes 是 evidence 经 Fold 归一后的最少字数（L2 评审 P2-6）。
const MinEvidenceRunes = 8

// sentenceEnds 是断句符号（全角与半角）；evidence 只许在结尾带它们。
const sentenceEnds = "。！？；!?;\n\r"

// Validate 校验非 UNKNOWN 的 judge 结果：分数在 1-3，evidence 是 evaluated 的一句原句（见 checkEvidence）。
// UNKNOWN 不经此校验（由调用方单独处理）。
func Validate(v Verdict, evaluated string) error {
	if v.Unknown() {
		return nil
	}
	if *v.Score < MinScore || *v.Score > MaxScore {
		return fmt.Errorf("%w: %d", ErrBadScore, *v.Score)
	}
	return checkEvidence(v.Evidence, evaluated)
}

// checkEvidence 校验 evidence 是 evaluated 的一句原句：非空且归一后是子串、不少于 MinEvidenceRunes 个字、
// 去掉结尾断句符号后中间不再有断句符号或换行。
func checkEvidence(evidence, evaluated string) error {
	ev := Fold(evidence)
	if ev == "" || !strings.Contains(Fold(evaluated), ev) {
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

// Fold 做 NFKC、转小写，只留字母与数字（去空白与标点）。与 evalkit/textnorm.Fold 同一实现（本包不得 import evalkit，
// 单测断言两者一致）。
func Fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(norm.NFKC.String(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
