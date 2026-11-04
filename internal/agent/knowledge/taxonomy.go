// Package knowledge 是知识层（L3 渐进式加载）。L0 先落 question_taxonomy 的加载：
// 四类问题域及其子类的稳定 code，供 L3 加载、L4 分类节点与评测样本校验共用。
package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TaxonomyItem 是一个分类或子类。
type TaxonomyItem struct {
	Code string `yaml:"code"`
	Name string `yaml:"name"`
}

// TaxonomyCategory 是一级问题域（施肥 / 植保 / 灌溉 / 灾害防备）。
type TaxonomyCategory struct {
	TaxonomyItem  `yaml:",inline"`
	Subcategories []TaxonomyItem `yaml:"subcategories"`
}

// Taxonomy 是整份枚举，加载后只读。
type Taxonomy struct {
	Version    int                `yaml:"version"`
	Status     string             `yaml:"status"`
	Crop       string             `yaml:"crop"`
	Region     string             `yaml:"region"`
	Categories []TaxonomyCategory `yaml:"categories"`
	// Stages 是评测分层的生育期维度（P2 L1-T）：Stages 枚举加 StageAny；生产路径不读。
	Stages []TaxonomyItem `yaml:"stages,omitempty"`
	// SampleTags 是评测样本的非问题域 tag（P2 L1-T：陷阱组 trap.* 与 user_sim）；生产路径不读。
	SampleTags []TaxonomyItem `yaml:"sample_tags,omitempty"`

	// code → 所属一级分类 code；一级分类映射到自身
	categoryOf map[string]string
	stages     map[string]bool
	sampleTags map[string]bool
}

// StageAny 是与生育期无关的问题（越界、跨农场、价格等）的分层取值。
const StageAny = "不限"

const taxonomySubcodeSep = "."

// LoadTaxonomy 从文件加载并校验。
func LoadTaxonomy(path string) (*Taxonomy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read taxonomy %s: %w", path, err)
	}
	t, err := ParseTaxonomy(raw)
	if err != nil {
		return nil, fmt.Errorf("parse taxonomy %s: %w", path, err)
	}
	return t, nil
}

// ParseTaxonomy 解析 YAML 并建立索引；code 为空、重复、子类未以「一级 code.」开头均报错。
func ParseTaxonomy(raw []byte) (*Taxonomy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	t := &Taxonomy{}
	if err := dec.Decode(t); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := t.index(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Taxonomy) index() error {
	if len(t.Categories) == 0 {
		return errors.New("no categories")
	}
	t.categoryOf = make(map[string]string)
	add := func(code, category string) error {
		if code == "" {
			return fmt.Errorf("empty code under category %q", category)
		}
		if _, dup := t.categoryOf[code]; dup {
			return fmt.Errorf("duplicate code %q", code)
		}
		t.categoryOf[code] = category
		return nil
	}
	for _, c := range t.Categories {
		if err := add(c.Code, c.Code); err != nil {
			return err
		}
		for _, s := range c.Subcategories {
			if !strings.HasPrefix(s.Code, c.Code+taxonomySubcodeSep) {
				return fmt.Errorf("subcategory %q must start with %q", s.Code, c.Code+taxonomySubcodeSep)
			}
			if err := add(s.Code, c.Code); err != nil {
				return err
			}
		}
	}
	var err error
	if t.stages, err = t.indexFlat("stages", t.Stages); err != nil {
		return err
	}
	if t.sampleTags, err = t.indexFlat("sample_tags", t.SampleTags); err != nil {
		return err
	}
	for code := range t.stages {
		if t.sampleTags[code] {
			return fmt.Errorf("code %q is both a stage and a sample tag", code)
		}
	}
	return nil
}

// indexFlat 索引一组平铺 code：不得为空、重复或与问题域 code 冲突。
func (t *Taxonomy) indexFlat(field string, items []TaxonomyItem) (map[string]bool, error) {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		switch {
		case it.Code == "":
			return nil, fmt.Errorf("empty code in %s", field)
		case m[it.Code]:
			return nil, fmt.Errorf("duplicate code %q in %s", it.Code, field)
		case t.Has(it.Code):
			return nil, fmt.Errorf("%s code %q clashes with a category code", field, it.Code)
		}
		m[it.Code] = true
	}
	return m, nil
}

// HasStage 判断 code 是否为分层生育期取值。
func (t *Taxonomy) HasStage(code string) bool { return t.stages[code] }

// HasSampleTag 判断 code 是否为样本 tag（陷阱组、user_sim）。
func (t *Taxonomy) HasSampleTag(code string) bool { return t.sampleTags[code] }

// Has 判断 code（一级或子类）是否在枚举内。
func (t *Taxonomy) Has(code string) bool {
	_, ok := t.categoryOf[code]
	return ok
}

// CategoryOf 返回 code 所属的一级分类 code。
func (t *Taxonomy) CategoryOf(code string) (string, bool) {
	c, ok := t.categoryOf[code]
	return c, ok
}
