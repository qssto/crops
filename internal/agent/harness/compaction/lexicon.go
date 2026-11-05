// Package compaction 是 P2 L6 长对话压缩（P2 §6.7）：挂在「上下文组装」步骤，把本会话之前轮次的
// 白名单事实（facts，代码从业务表提取，不交模型改写）、更早轮次的摘要与最近若干轮原文组装成会话块，
// 估算 token 超过阈值时压缩更早的自由文本，并在最终会话块上逐字段断言白名单（取值从源表重新读取）。
// 只依赖 agent 接缝与 model；读库由 repository.RunRepo 实现 Source。
package compaction

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrBadLexicon 表示约束词表文件非法。
var ErrBadLexicon = errors.New("compaction: user constraint lexicon invalid")

// Lexicon 是用户口述约束 / 禁忌词表（policy/user_constraint_lexicon.yaml）。
type Lexicon struct {
	Version  int      `yaml:"version"`
	Keywords []string `yaml:"keywords"`
	lower    []string
}

// LoadLexicon 读取并校验词表：version ≥ 1，关键词非空、去空白后不重复。
func LoadLexicon(path string) (*Lexicon, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compaction: read lexicon %s: %w", path, err)
	}
	var l Lexicon
	if err := yaml.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrBadLexicon, path, err)
	}
	if err := l.init(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &l, nil
}

// NewLexicon 由关键词直接构造词表（单测与评测装配用），校验同 LoadLexicon。
func NewLexicon(version int, keywords ...string) (*Lexicon, error) {
	l := Lexicon{Version: version, Keywords: keywords}
	if err := l.init(); err != nil {
		return nil, err
	}
	return &l, nil
}

func (l *Lexicon) init() error {
	if l.Version < 1 {
		return fmt.Errorf("%w: version must be >= 1, got %d", ErrBadLexicon, l.Version)
	}
	if len(l.Keywords) == 0 {
		return fmt.Errorf("%w: keywords empty", ErrBadLexicon)
	}
	seen := make(map[string]bool, len(l.Keywords))
	l.lower = make([]string, 0, len(l.Keywords))
	for i, k := range l.Keywords {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			return fmt.Errorf("%w: keywords[%d] empty", ErrBadLexicon, i)
		}
		if seen[k] {
			return fmt.Errorf("%w: keyword %q duplicated", ErrBadLexicon, k)
		}
		seen[k] = true
		l.lower = append(l.lower, k)
	}
	return nil
}

// sentenceBreaks 是切句标点（句末与分号）与换行。
const sentenceBreaks = "。！？!?；;\n"

// Constraints 返回 text 中命中任一关键词的句子（原句，只去首尾空白），按出现顺序、不去重。
func (l *Lexicon) Constraints(text string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(sentenceBreaks, r) }) {
		s = strings.TrimSpace(s)
		if s != "" && l.hit(s) {
			out = append(out, s)
		}
	}
	return out
}

func (l *Lexicon) hit(sentence string) bool {
	low := strings.ToLower(sentence)
	for _, k := range l.lower {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}
