// Package smoke 是 PR 冒烟子集（P2 §6.8 L7-a 功能任务 1）：eval/smoke/smoke.yaml 只列 sample_id，不复制样本。
// 覆盖口径：红线打分器集合由调用方传入的打分器 RedLine() 派生（新红线自动纳入），每个红线打分器在子集里
// 至少 MinPerGrader 条适用样本（适用按样本声明判定，见 Declared）；regression 不足时才允许从 redteam / constraint 补（supplement），全部公开样本都不足的
// 红线打分器须在 uncoverable 显式登记理由。
package smoke

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
)

const (
	// MinPerGrader 是每个红线打分器在冒烟子集里的最少适用样本数。
	MinPerGrader = 2
	// SuiteRegression 是冒烟子集的主套件。
	SuiteRegression = "regression"
	// SuiteRedteam / SuiteConstraint 是 regression 覆盖不足时允许补样本的公开套件（SupplementSuites）。
	SuiteRedteam    = "redteam"
	SuiteConstraint = "constraint"
	// TagUserSim 是 LLM 模拟用户样本的 tag：红线门禁不得含此类样本（§6.8 验收 4）。
	TagUserSim = "user_sim"
)

// SupplementSuites 是允许补样本的套件：expect.constraint 只出现在 constraint 套件，跨场探测只出现在 redteam。
var SupplementSuites = []string{SuiteRedteam, SuiteConstraint}

// Suites 是冒烟运行要选中的套件（--suite），覆盖 samples 与 supplement 两部分。
var Suites = append([]string{SuiteRegression}, SupplementSuites...)

// ErrCoverage 表示清单不满足覆盖或归属约束。
var ErrCoverage = errors.New("smoke: list violates coverage rules")

// Supplement 是从 redteam 补入的样本及其要补齐的红线打分器。
type Supplement struct {
	ID     string `yaml:"id"`
	Grader string `yaml:"grader"`
}

// Uncoverable 是全部公开可运行样本都不足 MinPerGrader 条的红线打分器及理由。
type Uncoverable struct {
	Grader string `yaml:"grader"`
	Reason string `yaml:"reason"`
}

// List 是 smoke.yaml 的内容。
type List struct {
	Samples     []string      `yaml:"samples"`
	Supplement  []Supplement  `yaml:"supplement"`
	Uncoverable []Uncoverable `yaml:"uncoverable"`
}

// Load 严格解析清单（未知字段报错），清单为空时报错。
func Load(path string) (*List, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("smoke: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var l List
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("smoke: parse %s: %w", path, err)
	}
	if len(l.IDs()) == 0 {
		return nil, fmt.Errorf("smoke: %s lists no sample", path)
	}
	return &l, nil
}

// IDs 返回 samples 与 supplement 的全部 sample_id（按出现顺序，含重复，重复由 Check 报出）。
func (l *List) IDs() []string {
	ids := slices.Clone(l.Samples)
	for _, s := range l.Supplement {
		ids = append(ids, s.ID)
	}
	return ids
}

// Applies 报告红线打分器 name 是否适用于样本：按 Declared 的样本声明规则判定；未登记规则的打分器不适用于任何样本。
func Applies(name string, s *sample.Sample) bool {
	rule, ok := Declared[name]
	return ok && rule(s)
}

// runnable 报告样本是否会被 run 实际运行且可进红线门禁：未隔离、不含 user_sim。
func runnable(s *sample.Sample) bool {
	_, quarantined := s.QuarantineReason()
	return !quarantined && !slices.Contains(s.Tags, TagUserSim)
}

// RedLines 返回 graders 中的红线打分器（保持顺序）。
func RedLines(graders []grader.Grader) []grader.Grader {
	var out []grader.Grader
	for _, g := range graders {
		if g.RedLine() {
			out = append(out, g)
		}
	}
	return out
}

// Check 校验清单。pool 是全部已加载样本（各套件），graders 是 run 所用打分器。违规一并返回（包 ErrCoverage）：
// 红线打分器须在 Declared 登记适用规则；
// 样本须存在、不重复、可运行；samples 须属 regression；supplement 须属 SupplementSuites、补的是红线打分器、适用于该样本，
// 且 regression 可运行样本里该打分器确实不足 MinPerGrader 条；uncoverable 须是红线打分器、写明理由，
// 且 regression + SupplementSuites 可运行样本里确实不足；其余每个红线打分器在子集里至少 MinPerGrader 条适用样本。
func Check(l *List, pool []*sample.Sample, graders []grader.Grader) error {
	byID := make(map[string]*sample.Sample, len(pool))
	for _, s := range pool {
		byID[s.ID] = s
	}
	chosen, errs := resolve(l, byID)
	red := make(map[string]bool)
	for _, g := range RedLines(graders) {
		red[g.Name()] = true
	}
	regression := filter(pool, SuiteRegression)
	public := slices.Clone(regression)
	for _, suite := range SupplementSuites {
		public = append(public, filter(pool, suite)...)
	}
	for _, sup := range l.Supplement {
		errs = append(errs, checkSupplement(sup, byID[sup.ID], red, regression)...)
	}
	uncoverable := make(map[string]bool, len(l.Uncoverable))
	for _, u := range l.Uncoverable {
		errs = append(errs, checkUncoverable(u, red, public)...)
		uncoverable[u.Grader] = true
	}
	for _, g := range RedLines(graders) {
		if _, ok := Declared[g.Name()]; !ok {
			errs = append(errs, fmt.Sprintf("red-line grader %s: no applicability rule in smoke.Declared", g.Name()))
			continue
		}
		if uncoverable[g.Name()] {
			continue
		}
		if n := countApplicable(g.Name(), chosen); n < MinPerGrader {
			errs = append(errs, fmt.Sprintf("red-line grader %s: %d applicable samples, want >= %d", g.Name(), n, MinPerGrader))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w:\n  %s", ErrCoverage, strings.Join(errs, "\n  "))
	}
	return nil
}

// resolve 按清单顺序取样本：重复、不存在、不可运行的记违规；samples 段须属 regression。
func resolve(l *List, byID map[string]*sample.Sample) ([]*sample.Sample, []string) {
	var errs []string
	chosen, seen := make([]*sample.Sample, 0, len(l.IDs())), make(map[string]bool)
	for _, id := range l.IDs() {
		s, ok := byID[id]
		switch {
		case seen[id]:
			errs = append(errs, fmt.Sprintf("%s: listed twice", id))
		case !ok:
			errs = append(errs, fmt.Sprintf("%s: no such sample", id))
		case !runnable(s):
			errs = append(errs, fmt.Sprintf("%s: quarantined or tagged %s, cannot gate red lines", id, TagUserSim))
		default:
			chosen = append(chosen, s)
		}
		seen[id] = true
	}
	for _, id := range l.Samples {
		if s, ok := byID[id]; ok && s.Suite != SuiteRegression {
			errs = append(errs, fmt.Sprintf("%s: samples must be in %s, got %s", id, SuiteRegression, s.Suite))
		}
	}
	return chosen, errs
}

// checkSupplement 校验一条 supplement；s 为 nil（样本不存在，已由 resolve 报出）时跳过归属检查。
func checkSupplement(sup Supplement, s *sample.Sample, red map[string]bool, regression []*sample.Sample) []string {
	if !red[sup.Grader] {
		return []string{fmt.Sprintf("supplement %s: %s is not a red-line grader", sup.ID, sup.Grader)}
	}
	var errs []string
	if n := countApplicable(sup.Grader, regression); n >= MinPerGrader {
		errs = append(errs, fmt.Sprintf("supplement %s: %s already has %d runnable samples for %s, use them", sup.ID, SuiteRegression, n, sup.Grader))
	}
	if s == nil {
		return errs
	}
	if !slices.Contains(SupplementSuites, s.Suite) {
		errs = append(errs, fmt.Sprintf("supplement %s: must be in %s, got %s", sup.ID, strings.Join(SupplementSuites, "|"), s.Suite))
	}
	if !Applies(sup.Grader, s) {
		errs = append(errs, fmt.Sprintf("supplement %s: %s does not apply", sup.ID, sup.Grader))
	}
	return errs
}

// checkUncoverable 校验一条 uncoverable：须是红线打分器、有理由，且 public 可运行样本里确实不足 MinPerGrader 条。
func checkUncoverable(u Uncoverable, red map[string]bool, public []*sample.Sample) []string {
	switch {
	case !red[u.Grader]:
		return []string{fmt.Sprintf("uncoverable %s: not a red-line grader", u.Grader)}
	case strings.TrimSpace(u.Reason) == "":
		return []string{fmt.Sprintf("uncoverable %s: reason is empty", u.Grader)}
	}
	if n := countApplicable(u.Grader, public); n >= MinPerGrader {
		return []string{fmt.Sprintf("uncoverable %s: %s+%s has %d runnable applicable samples, list them instead",
			u.Grader, SuiteRegression, strings.Join(SupplementSuites, "+"), n)}
	}
	return nil
}

// countApplicable 数 samples 中可运行且红线打分器 name 适用的样本。
func countApplicable(name string, samples []*sample.Sample) int {
	n := 0
	for _, s := range samples {
		if runnable(s) && Applies(name, s) {
			n++
		}
	}
	return n
}

// filter 返回属于 suite 的样本。
func filter(samples []*sample.Sample, suite string) []*sample.Sample {
	var out []*sample.Sample
	for _, s := range samples {
		if s.Suite == suite {
			out = append(out, s)
		}
	}
	return out
}
