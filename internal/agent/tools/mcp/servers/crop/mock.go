package crop

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/clock"
)

// monthsPerYear 是月均温表的长度。
const monthsPerYear = 12

// Data 是 configs/mock/crop.yaml。
type Data struct {
	Source           string               `yaml:"source"`
	UpdatedAt        time.Time            `yaml:"updated_at"`          // 农技资料版本时刻（品种/病虫谱/阈值的数据时间）
	GDDBaseC         float64              `yaml:"gdd_base_c"`          // 有效积温基温
	MonthlyMeanTempC []float64            `yaml:"monthly_mean_temp_c"` // 当地 1..12 月日均温气候值
	Stages           []StageDef           `yaml:"stages"`              // 按 gdd_from 升序，首项为 0
	Varieties        map[string]Variety   `yaml:"varieties"`
	Fields           map[string]FieldCrop `yaml:"fields"` // 地块编码 → 种植信息（与 configs/seed/fields.yaml 一致）
	Spectrum         []SpectrumEntry      `yaml:"spectrum"`
	Thresholds       []Threshold          `yaml:"thresholds"`
}

// StageDef 是一个（子）生育期的起始积温。
type StageDef struct {
	Stage    string  `yaml:"stage"`
	SubStage string  `yaml:"sub_stage,omitempty"`
	GDDFrom  float64 `yaml:"gdd_from"`
}

// Variety 是品种特性。
type Variety struct {
	Type       string            `yaml:"type"`
	GrowthDays int               `yaml:"growth_days"`
	Resistance map[string]string `yaml:"resistance"`
	Notes      []string          `yaml:"notes"`
}

// FieldCrop 是地块的种植信息。
type FieldCrop struct {
	Crop       string `yaml:"crop"`
	Variety    string `yaml:"variety"`
	SowingDate string `yaml:"sowing_date"`
}

// SpectrumEntry 是一个防治对象：risk 按生育期给出风险，未列出的生育期不关注。
type SpectrumEntry struct {
	Target string            `yaml:"target"`
	Name   string            `yaml:"name"`
	Kind   string            `yaml:"kind"`
	Risk   map[string]string `yaml:"risk"`
}

// Threshold 是某防治对象在若干生育期的防治指标。
type Threshold struct {
	Target     string      `yaml:"target"`
	Stages     []string    `yaml:"stages"`
	Logic      string      `yaml:"logic"`
	Indicators []Indicator `yaml:"indicators"`
}

// Indicator 是一条指标。
type Indicator struct {
	Metric string  `yaml:"metric" json:"metric"`
	Op     string  `yaml:"op" json:"op"`
	Value  float64 `yaml:"value" json:"value"`
	Unit   string  `yaml:"unit" json:"unit"`
}

func (d Data) validate() error {
	var errs []error
	add := func(bad bool, format string, a ...any) {
		if bad {
			errs = append(errs, fmt.Errorf(format, a...))
		}
	}
	add(d.Source == "", "source is required")
	add(d.UpdatedAt.IsZero(), "updated_at is required")
	add(len(d.MonthlyMeanTempC) != monthsPerYear, "monthly_mean_temp_c must have %d values", monthsPerYear)
	add(len(d.Stages) == 0 || d.Stages[0].GDDFrom != 0, "stages must start at gdd_from 0")
	for i, s := range d.Stages {
		add(!slices.Contains(kit.Stages, s.Stage), "stages[%d]: unknown stage %q", i, s.Stage)
		add(i > 0 && s.GDDFrom <= d.Stages[i-1].GDDFrom, "stages[%d]: gdd_from must increase", i)
	}
	for code, f := range d.Fields {
		_, err := time.Parse(time.DateOnly, f.SowingDate)
		add(err != nil, "fields.%s.sowing_date %q must be YYYY-MM-DD", code, f.SowingDate)
		_, ok := d.Varieties[f.Variety]
		add(!ok, "fields.%s: unknown variety %q", code, f.Variety)
	}
	for _, s := range d.Spectrum {
		for st := range s.Risk {
			add(!slices.Contains(kit.Stages, st), "spectrum %s: unknown stage %q", s.Target, st)
		}
	}
	for _, t := range d.Thresholds {
		add(len(t.Indicators) == 0, "threshold %s: indicators must be non-empty", t.Target)
		for _, st := range t.Stages {
			add(!slices.Contains(kit.Stages, st), "threshold %s: unknown stage %q", t.Target, st)
		}
	}
	return errors.Join(errs...)
}

type mock struct {
	data Data
	clk  clock.Clock
	dir  kit.FieldDirectory
}

// LoadMock 读取 mock 数据并构造 mock Server。
func LoadMock(path string, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	var d Data
	if err := kit.LoadYAML(path, &d); err != nil {
		return nil, err
	}
	return NewMock(d, clk, dir)
}

// NewMock 用给定数据构造 mock Server。
func NewMock(d Data, clk clock.Clock, dir kit.FieldDirectory) (mcp.Server, error) {
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("crop mock data: %w", err)
	}
	m := &mock{data: d, clk: clk, dir: dir}
	return kit.NewServer(Name, Specs(), map[string]kit.Handler{
		ToolPhenology: m.phenology, ToolVariety: m.variety, ToolPestSpectrum: m.pestSpectrum, ToolThreshold: m.threshold,
	})
}
