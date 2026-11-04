package compare

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrFactorIncomplete 表示候选因子登记表有缺项（五项缺一项不进入排期，P2 §8）。
var ErrFactorIncomplete = errors.New("compare: factor entry incomplete")

// FactorEntry 是 eval/gates/factors.yaml 的一行（§8 表格五项 + 名字与来源层）。
type FactorEntry struct {
	Name      string   `yaml:"name"`
	Layer     string   `yaml:"layer"`
	Baseline  string   `yaml:"baseline"`
	Candidate string   `yaml:"candidate"`
	Metrics   []string `yaml:"metrics"`
	Suites    []string `yaml:"suites"`
	Rollback  string   `yaml:"rollback"`
}

// FactorTable 是候选因子登记表。
type FactorTable struct {
	Factors []FactorEntry `yaml:"factors"`
}

// missing 返回该行为空的必填项名。
func (e FactorEntry) missing() []string {
	fields := []struct {
		name  string
		empty bool
	}{
		{"name", strings.TrimSpace(e.Name) == ""},
		{"baseline", strings.TrimSpace(e.Baseline) == ""},
		{"candidate", strings.TrimSpace(e.Candidate) == ""},
		{"metrics", blank(e.Metrics)},
		{"suites", blank(e.Suites)},
		{"rollback", strings.TrimSpace(e.Rollback) == ""},
	}
	var out []string
	for _, f := range fields {
		if f.empty {
			out = append(out, f.name)
		}
	}
	return out
}

func blank(xs []string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == "" {
			return true
		}
	}
	return len(xs) == 0
}

// Validate 要求至少一行、名字不重复且每行五项齐全；所有缺项一次报出。
func (t *FactorTable) Validate() error {
	if len(t.Factors) == 0 {
		return fmt.Errorf("%w: no factors", ErrFactorIncomplete)
	}
	var problems []string
	seen := map[string]bool{}
	for i, e := range t.Factors {
		if m := e.missing(); len(m) > 0 {
			problems = append(problems, fmt.Sprintf("#%d %s: missing %s", i+1, e.Name, strings.Join(m, ",")))
		}
		if seen[e.Name] {
			problems = append(problems, fmt.Sprintf("#%d %s: duplicate name", i+1, e.Name))
		}
		seen[e.Name] = true
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrFactorIncomplete, strings.Join(problems, "; "))
	}
	return nil
}

// ParseFactorTable 解析并校验登记表（未知字段报错，防止拼错字段名被当作缺项之外的静默忽略）。
func ParseFactorTable(raw []byte) (*FactorTable, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var t FactorTable
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("compare: parse factor table: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// LoadFactorTable 读取并校验 eval/gates/factors.yaml。
func LoadFactorTable(path string) (*FactorTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compare: read %s: %w", path, err)
	}
	return ParseFactorTable(raw)
}
