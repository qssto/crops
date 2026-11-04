// Package coverage 是评测集分层覆盖报表（P2 L1-T）：按 作物 × 生育期 × 问题类型 × 难度 计数，
// 对照 eval/coverage/targets.yaml 的目标给出缺口（补写清单）、拒答三来源占比与陷阱组占比。
package coverage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// RefuseSources 是三类拒答来源（P2 §6.2）：知识未覆盖、数据缺失、超出作物范围。其余 refuse_reason 只计入总拒答。
var RefuseSources = []string{"KB_MISS", "DATA_MISSING", "OUT_OF_SCOPE"}

// TrapGroups 是四个陷阱组 tag（与 eval/taxonomy.yaml sample_tags 一致）。
var TrapGroups = []string{sample.TrapSimilarPest, sample.TrapOverdose, sample.TrapCrossCrop, sample.TrapStaleKB}

// Targets 是覆盖目标（eval/coverage/targets.yaml）。
type Targets struct {
	Version  int          `yaml:"version"`
	Crop     string       `yaml:"crop"`
	TotalMin int          `yaml:"total_min"`
	Cells    []CellTarget `yaml:"cells"`
	Refuse   RatioTarget  `yaml:"refuse"`
	Traps    RatioTarget  `yaml:"traps"`
}

// CellTarget 是一行目标：一个 生育期 × 问题类型 下三档难度各需多少条。
type CellTarget struct {
	Stage  string `yaml:"stage"`
	Topic  string `yaml:"topic"`
	Easy   int    `yaml:"easy"`
	Medium int    `yaml:"medium"`
	Hard   int    `yaml:"hard"`
}

// ByDifficulty 返回某档难度的目标条数。
func (c CellTarget) ByDifficulty(d string) int {
	switch d {
	case sample.DifficultyEasy:
		return c.Easy
	case sample.DifficultyMedium:
		return c.Medium
	case sample.DifficultyHard:
		return c.Hard
	}
	return 0
}

// Sum 返回该行三档目标之和。
func (c CellTarget) Sum() int { return c.Easy + c.Medium + c.Hard }

// RatioTarget 是占比下限加分项最少条数（拒答按来源、陷阱按组）。
type RatioTarget struct {
	RatioMin float64        `yaml:"ratio_min"`
	Min      map[string]int `yaml:"min"`
}

// CellKey 是分层格子的行键（生育期 × 问题类型）。
type CellKey struct {
	Stage string
	Topic string
}

// LoadTargets 读取并校验目标文件。
func LoadTargets(path string, tx *knowledge.Taxonomy) (*Targets, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read coverage targets %s: %w", path, err)
	}
	t, err := ParseTargets(raw, tx)
	if err != nil {
		return nil, fmt.Errorf("coverage targets %s: %w", path, err)
	}
	return t, nil
}

// ParseTargets 严格解析目标：stage / topic 须在 taxonomy 内、行不重复、条数非负，
// 拒答来源与陷阱组须已登记，格子目标之和须不小于 total_min。
func ParseTargets(raw []byte, tx *knowledge.Taxonomy) (*Targets, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	t := &Targets{}
	if err := dec.Decode(t); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty targets file")
		}
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := t.validate(tx); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Targets) validate(tx *knowledge.Taxonomy) error {
	if t.Crop != tx.Crop {
		return fmt.Errorf("crop %q must be taxonomy crop %q", t.Crop, tx.Crop)
	}
	seen := make(map[CellKey]bool, len(t.Cells))
	sum := 0
	for i, c := range t.Cells {
		where := fmt.Sprintf("cells[%d] (%s × %s)", i, c.Stage, c.Topic)
		if !tx.HasStage(c.Stage) {
			return fmt.Errorf("%s: stage is not a taxonomy stage", where)
		}
		if cat, ok := tx.CategoryOf(c.Topic); !ok || cat == c.Topic {
			return fmt.Errorf("%s: topic must be a taxonomy subcategory code", where)
		}
		if c.Easy < 0 || c.Medium < 0 || c.Hard < 0 {
			return fmt.Errorf("%s: targets must be >= 0", where)
		}
		k := CellKey{c.Stage, c.Topic}
		if seen[k] {
			return fmt.Errorf("%s: duplicate row", where)
		}
		seen[k] = true
		sum += c.Sum()
	}
	if t.TotalMin <= 0 || sum < t.TotalMin {
		return fmt.Errorf("total_min %d must be > 0 and <= sum of cell targets %d", t.TotalMin, sum)
	}
	if err := checkRatio("refuse", t.Refuse, RefuseSources); err != nil {
		return err
	}
	return checkRatio("traps", t.Traps, TrapGroups)
}

func checkRatio(field string, r RatioTarget, known []string) error {
	if r.RatioMin < 0 || r.RatioMin > 1 {
		return fmt.Errorf("%s.ratio_min %v must be in [0, 1]", field, r.RatioMin)
	}
	for k, n := range r.Min {
		if !slices.Contains(known, k) {
			return fmt.Errorf("%s.min key %q not in %v", field, k, known)
		}
		if n < 0 {
			return fmt.Errorf("%s.min[%s] must be >= 0", field, k)
		}
	}
	return nil
}
