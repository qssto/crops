package policy

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// TextsVersion 是本包支持的 configs/texts/*.yaml 版本。
const TextsVersion = 1

// ErrBadTexts 是文案表不合法（加载期，启动失败）。
var ErrBadTexts = errors.New("policy: invalid texts")

// Texts 是一份用户可读文案表（configs/texts/zh-CN.yaml）：key → 文案。构造后只读，并发安全。
type Texts struct {
	locale string
	texts  map[string]string
}

type textsFile struct {
	Version int               `yaml:"version"`
	Locale  string            `yaml:"locale"`
	Texts   map[string]string `yaml:"texts"`
}

// LoadTexts 读取并校验文案表。
func LoadTexts(path string) (*Texts, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: read texts: %w", err)
	}
	t, err := ParseTexts(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// ParseTexts 解析文案表：version 受支持、locale 非空、至少一条且每条 key / 文案非空。
func ParseTexts(b []byte) (*Texts, error) {
	var f textsFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrBadTexts, err)
	}
	switch {
	case f.Version != TextsVersion:
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadTexts, f.Version, TextsVersion)
	case f.Locale == "":
		return nil, fmt.Errorf("%w: locale is required", ErrBadTexts)
	case len(f.Texts) == 0:
		return nil, fmt.Errorf("%w: no texts", ErrBadTexts)
	}
	for k, v := range f.Texts {
		if k == "" || v == "" {
			return nil, fmt.Errorf("%w: empty key or text %q", ErrBadTexts, k)
		}
	}
	return &Texts{locale: f.Locale, texts: f.Texts}, nil
}

// Locale 是文案表的语言（zh-CN）。
func (t *Texts) Locale() string { return t.locale }

// Get 按 key 取文案。
func (t *Texts) Get(key string) (string, bool) {
	s, ok := t.texts[key]
	return s, ok
}
