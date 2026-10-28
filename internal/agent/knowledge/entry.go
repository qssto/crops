package knowledge

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Wildcard 表示条目对该维度（crop / region / stage）不加限制。
const Wildcard = "*"

// AuthorityLevel 是条目权威等级，加载排序按 Rank 从高到低。
type AuthorityLevel string

// 权威等级（knowledge/README.md §2）。
const (
	AuthorityNational   AuthorityLevel = "国标"
	AuthorityLocal      AuthorityLevel = "地标"
	AuthorityEnterprise AuthorityLevel = "企业标准"
	AuthorityExpert     AuthorityLevel = "专家经验"
)

var authorityRank = map[AuthorityLevel]int{
	AuthorityNational: 4, AuthorityLocal: 3, AuthorityEnterprise: 2, AuthorityExpert: 1,
}

// Rank 返回排序权重，越大越权威；未知等级为 0。
func (a AuthorityLevel) Rank() int { return authorityRank[a] }

// Stages 是生育期枚举（knowledge/README.md §3），与 crop.phenology 的 stage 共用。
var Stages = []string{"秧苗期", "返青期", "分蘖期", "拔节孕穗期", "抽穗扬花期", "灌浆结实期", "成熟期"}

const (
	authorClaudeDraft   = "claude-draft"
	reviewPendingExpert = "pending_expert"
	reviewSigned        = "signed"
)

var (
	kbSegmentPattern = regexp.MustCompile(`^[a-z0-9]+([_-][a-z0-9]+)*$`)
	versionPattern   = regexp.MustCompile(`^v[1-9][0-9]*$`)
	knownCalcs       = map[string]bool{"Dosage": true, "Dilution": true, "PHIDate": true, "Fertilizer": true, "WaterVolume": true}
	knownOps         = map[string]bool{">=": true, ">": true, "<=": true, "<": true, "==": true}
)

// Entry 是一个知识条目：front-matter 结构化字段 + 正文。加载后只读，调用方不得修改。
type Entry struct {
	KBID           string         `yaml:"kb_id" json:"kb_id"`
	Title          string         `yaml:"title" json:"title"`
	Crop           string         `yaml:"crop" json:"crop"`
	Stage          []string       `yaml:"stage" json:"stage"`
	Region         string         `yaml:"region" json:"region"`
	Category       []string       `yaml:"category" json:"category"`
	Topics         []string       `yaml:"topics" json:"topics"`
	Target         string         `yaml:"target,omitempty" json:"target,omitempty"`
	AuthorityLevel AuthorityLevel `yaml:"authority_level" json:"authority_level"`
	Version        string         `yaml:"version" json:"version"`
	Author         string         `yaml:"author" json:"author"`
	ReviewStatus   string         `yaml:"review_status" json:"review_status"`
	EffectiveFrom  string         `yaml:"effective_from" json:"effective_from"`
	Thresholds     []Threshold    `yaml:"thresholds,omitempty" json:"thresholds,omitempty"`
	Params         []Param        `yaml:"params,omitempty" json:"params,omitempty"`
	Formulas       []Formula      `yaml:"formulas,omitempty" json:"formulas,omitempty"`
	Basis          []Basis        `yaml:"basis" json:"basis"`

	// File 是条目所在文件（相对 knowledge/），Body 是正文 Markdown；均不来自 front-matter。
	File string `yaml:"-" json:"file"`
	Body string `yaml:"-" json:"body"`
}

// Threshold 是指标与阈值（README §5.1）。
type Threshold struct {
	ID           string   `yaml:"id" json:"id"`
	Label        string   `yaml:"label" json:"label"`
	Metric       string   `yaml:"metric" json:"metric"`
	Op           string   `yaml:"op" json:"op"`
	Value        float64  `yaml:"value" json:"value"`
	Unit         string   `yaml:"unit" json:"unit"`
	Stage        []string `yaml:"stage,omitempty" json:"stage,omitempty"`
	DurationDays int      `yaml:"duration_days,omitempty" json:"duration_days,omitempty"`
	Action       string   `yaml:"action" json:"action"`
}

// Param 是 calc 参数（README §5.2）：点值 Value 或区间 Min / Max，可带适用条件 When。
type Param struct {
	ID    string         `yaml:"id" json:"id"`
	Label string         `yaml:"label" json:"label"`
	Value *float64       `yaml:"value,omitempty" json:"value,omitempty"`
	Min   *float64       `yaml:"min,omitempty" json:"min,omitempty"`
	Max   *float64       `yaml:"max,omitempty" json:"max,omitempty"`
	Unit  string         `yaml:"unit" json:"unit"`
	When  map[string]any `yaml:"when,omitempty" json:"when,omitempty"`
}

// Formula 是公式声明（README §5.3）；calc 在 Go 中实现，Expr 供人读与单测核对。
type Formula struct {
	ID     string        `yaml:"id" json:"id"`
	Calc   string        `yaml:"calc" json:"calc"`
	Expr   string        `yaml:"expr" json:"expr"`
	Inputs []string      `yaml:"inputs" json:"inputs"`
	Output FormulaOutput `yaml:"output" json:"output"`
}

// FormulaOutput 是公式输出名与单位。
type FormulaOutput struct {
	Name string `yaml:"name" json:"name"`
	Unit string `yaml:"unit" json:"unit"`
}

// Basis 是数值依据来源（README §5.4）。
type Basis struct {
	Source   string         `yaml:"source" json:"source"`
	Level    AuthorityLevel `yaml:"level" json:"level"`
	Verified bool           `yaml:"verified" json:"verified"`
}

// Ref 返回引用写法 kb:<kb_id>@<version>。
func (e *Entry) Ref() string { return CitationRef(e.KBID, e.Version) }

// FormulaRef 返回 calc trace 用的公式引用 <kb_id>@<version>#<formula id>。
func (e *Entry) FormulaRef(formulaID string) string {
	return e.KBID + "@" + e.Version + "#" + formulaID
}

// Formula 按 id 查公式。
func (e *Entry) Formula(id string) (Formula, bool) {
	for _, f := range e.Formulas {
		if f.ID == id {
			return f, true
		}
	}
	return Formula{}, false
}

// Param 按 id 与条件查参数：when 为空时只匹配无条件参数；否则要求参数的 When 与之完全相等。
func (e *Entry) Param(id string, when map[string]any) (Param, bool) {
	for _, p := range e.Params {
		if p.ID == id && sameWhen(p.When, when) {
			return p, true
		}
	}
	return Param{}, false
}

func sameWhen(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || fmt.Sprint(v) != fmt.Sprint(w) {
			return false
		}
	}
	return true
}

// CitationRef 拼接引用 kb:<kb_id>@<version>。
func CitationRef(kbID, version string) string { return "kb:" + kbID + "@" + version }

// ParseCitationRef 解析 kb:<kb_id>@<version>。
func ParseCitationRef(ref string) (kbID, version string, err error) {
	rest, ok := strings.CutPrefix(ref, "kb:")
	if !ok {
		return "", "", fmt.Errorf("citation %q must start with kb:", ref)
	}
	kbID, version, ok = strings.Cut(rest, "@")
	if !ok || kbID == "" || version == "" {
		return "", "", fmt.Errorf("citation %q must be kb:<kb_id>@<version>", ref)
	}
	return kbID, version, nil
}

// FileOf 由 kb_id 推出所在文件（README §4）：rice/pest/<slug> → rice/pest/<slug>.md，其余 → <seg1>/<seg2>.md。
func FileOf(kbID string) (string, error) {
	segs := strings.Split(kbID, "/")
	if len(segs) != 3 {
		return "", fmt.Errorf("kb_id %q must have 3 segments", kbID)
	}
	for _, s := range segs {
		if !kbSegmentPattern.MatchString(s) {
			return "", fmt.Errorf("kb_id %q: segment %q must match %s", kbID, s, kbSegmentPattern)
		}
	}
	if segs[1] == "pest" {
		return kbID + ".md", nil
	}
	return segs[0] + "/" + segs[1] + ".md", nil
}

// ParseEntries 按「单独一行的 ---」切分一个 .md 文件，依次解析 front-matter 与正文并校验。
func ParseEntries(file string, raw []byte) ([]*Entry, error) {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return nil, fmt.Errorf("%s: must start with ---", file)
	}
	// parts[0] 为首个 --- 之前的空串，其后交替为 front-matter、正文
	var parts []string
	var cur []string
	for _, ln := range lines[1:] {
		if ln == "---" {
			parts = append(parts, strings.Join(cur, "\n"))
			cur = cur[:0]
			continue
		}
		cur = append(cur, ln)
	}
	parts = append(parts, strings.Join(cur, "\n"))
	if len(parts)%2 != 0 {
		return nil, fmt.Errorf("%s: unbalanced --- (front-matter without body)", file)
	}
	var out []*Entry
	var errs []error
	for i := 0; i < len(parts); i += 2 {
		e, err := parseFrontMatter(parts[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s entry #%d: %w", file, i/2+1, err))
			continue
		}
		e.File, e.Body = file, strings.TrimSpace(parts[i+1])
		if err := e.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", file, e.KBID, err))
			continue
		}
		out = append(out, e)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func parseFrontMatter(s string) (*Entry, error) {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(s)))
	dec.KnownFields(true)
	e := &Entry{}
	if err := dec.Decode(e); err != nil {
		return nil, fmt.Errorf("front-matter: %w", err)
	}
	return e, nil
}
