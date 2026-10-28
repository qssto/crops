package sample

import (
	"fmt"
	"sort"
	"strings"
)

// Violation 是一条静态校验失败。
type Violation struct {
	SampleID string
	Path     string
	Rule     string
	Msg      string
}

// Subject 返回违规对象的标识：优先样本 ID，解析失败等无 ID 时用文件路径。
func (v Violation) Subject() string {
	if v.SampleID != "" {
		return v.SampleID
	}
	return v.Path
}

func (v Violation) String() string {
	return fmt.Sprintf("[%s] %s: %s", v.Rule, v.Subject(), v.Msg)
}

// Rule 是一条静态校验规则。规则拿到整个样本集，既可逐条检查，也可做跨样本检查（ID 唯一、配比）。
type Rule interface {
	Name() string
	Check(samples []*Sample) []Violation
}

// SampleCheck 检查单个样本，返回问题描述列表。
type SampleCheck func(s *Sample) []string

type perSampleRule struct {
	name  string
	check SampleCheck
}

// PerSample 把逐样本检查函数包装成 Rule。L3 的 must_cite / product_in 规则也用它追加。
func PerSample(name string, check SampleCheck) Rule {
	return perSampleRule{name: name, check: check}
}

func (r perSampleRule) Name() string { return r.name }

func (r perSampleRule) Check(samples []*Sample) []Violation {
	var out []Violation
	for _, s := range samples {
		for _, msg := range r.check(s) {
			out = append(out, Violation{SampleID: s.ID, Path: s.Path, Rule: r.name, Msg: msg})
		}
	}
	return out
}

// Validator 依次执行规则列表。
type Validator struct {
	rules []Rule
}

// NewValidator 用给定规则构造校验器。
func NewValidator(rules ...Rule) *Validator {
	return &Validator{rules: rules}
}

// Validate 执行全部规则，不在首个失败处停止，便于作者一次看到所有问题。
func (v *Validator) Validate(samples []*Sample) Report {
	var r Report
	for _, rule := range v.rules {
		r.Violations = append(r.Violations, rule.Check(samples)...)
	}
	return r
}

// Report 是校验结果。
type Report struct {
	Violations []Violation
}

// OK 表示没有违规。
func (r Report) OK() bool { return len(r.Violations) == 0 }

// FailedIDs 返回去重排序后的失败样本 ID（无 ID 时为文件路径）。
func (r Report) FailedIDs() []string {
	seen := make(map[string]struct{}, len(r.Violations))
	ids := make([]string, 0, len(r.Violations))
	for _, v := range r.Violations {
		id := v.Subject()
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ValidationError 是校验失败时返回的错误，携带完整报告。
type ValidationError struct {
	Report Report
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	ids := e.Report.FailedIDs()
	fmt.Fprintf(&b, "sample validation failed: %d violation(s) in %d sample(s): %s",
		len(e.Report.Violations), len(ids), strings.Join(ids, ", "))
	for _, v := range e.Report.Violations {
		b.WriteString("\n  ")
		b.WriteString(v.String())
	}
	return b.String()
}

// LoadAndValidate 加载 root 下全部样本并校验；解析失败与规则违规合并为一个 *ValidationError。
func LoadAndValidate(root string, v *Validator) ([]*Sample, error) {
	samples, parseProblems, err := LoadDir(root)
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 && len(parseProblems) == 0 {
		return nil, fmt.Errorf("no sample found under %s", root)
	}
	report := v.Validate(samples)
	report.Violations = append(parseProblems, report.Violations...)
	if !report.OK() {
		return nil, &ValidationError{Report: report}
	}
	return samples, nil
}
