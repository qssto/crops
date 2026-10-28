package field

import (
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/clock"
)

// Data 是 configs/mock/field.yaml：按地块编码组织（与 configs/seed/fields.yaml 一致）。
type Data struct {
	Source string               `yaml:"source"`
	Fields map[string]FieldData `yaml:"fields"`
}

// FieldData 是一个地块的全部 mock 数据。农事与调查按日期过滤：晚于当前日期的记录视为尚未发生。
type FieldData struct {
	Profile    Profile     `yaml:"profile"`
	Operations []Operation `yaml:"operations"`
	Surveys    []Survey    `yaml:"surveys"`
	Soil       *Soil       `yaml:"soil,omitempty"`
}

// Profile 是地块档案。
type Profile struct {
	UpdatedAt      time.Time `yaml:"updated_at" json:"-"`
	AreaMu         float64   `yaml:"area_mu" json:"area_mu"`
	Crop           string    `yaml:"crop" json:"crop"`
	Variety        string    `yaml:"variety" json:"variety"`
	SowingDate     string    `yaml:"sowing_date" json:"sowing_date"`
	TransplantDate string    `yaml:"transplant_date,omitempty" json:"transplant_date,omitempty"`
	SoilTexture    string    `yaml:"soil_texture" json:"soil_texture"`
	Irrigation     string    `yaml:"irrigation" json:"irrigation"`
}

// Operation 是一条农事记录。
type Operation struct {
	Date     string  `yaml:"date" json:"date"`
	Kind     string  `yaml:"kind" json:"kind"`
	StageTag string  `yaml:"stage_tag,omitempty" json:"stage_tag,omitempty"`
	Product  string  `yaml:"product,omitempty" json:"product,omitempty"`
	Amount   float64 `yaml:"amount,omitempty" json:"amount,omitempty"`
	Unit     string  `yaml:"unit,omitempty" json:"unit,omitempty"`
	Note     string  `yaml:"note,omitempty" json:"note,omitempty"`
}

// Survey 是一次病虫害田间调查。
type Survey struct {
	Date         string        `yaml:"date" json:"survey_date"`
	Method       string        `yaml:"method" json:"method"`
	Observations []Observation `yaml:"observations" json:"observations"`
}

// Observation 是一条调查观测值。
type Observation struct {
	Target string  `yaml:"target" json:"target"`
	Name   string  `yaml:"name" json:"name"`
	Metric string  `yaml:"metric" json:"metric"`
	Value  float64 `yaml:"value" json:"value"`
	Unit   string  `yaml:"unit" json:"unit"`
}

// Soil 是土壤检测结果。
type Soil struct {
	SampledAt     string  `yaml:"sampled_at" json:"sampled_at"`
	PH            float64 `yaml:"ph" json:"ph"`
	OrganicMatter float64 `yaml:"organic_matter_g_kg" json:"organic_matter_g_kg"`
	AlkaliN       float64 `yaml:"alkali_n_mg_kg" json:"alkali_n_mg_kg"`
	OlsenP        float64 `yaml:"olsen_p_mg_kg" json:"olsen_p_mg_kg"`
	AvailableK    float64 `yaml:"available_k_mg_kg" json:"available_k_mg_kg"`
	NLevel        string  `yaml:"n_level" json:"n_level"`
}

func (d Data) validate() error {
	var errs []error
	if d.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	checkDate := func(where, s string) {
		if _, err := time.Parse(time.DateOnly, s); err != nil {
			errs = append(errs, fmt.Errorf("%s: date %q must be YYYY-MM-DD", where, s))
		}
	}
	for code, f := range d.Fields {
		if f.Profile.UpdatedAt.IsZero() || f.Profile.AreaMu <= 0 {
			errs = append(errs, fmt.Errorf("fields.%s.profile: updated_at and area_mu > 0 are required", code))
		}
		checkDate(code+".profile.sowing_date", f.Profile.SowingDate)
		for i, op := range f.Operations {
			checkDate(fmt.Sprintf("%s.operations[%d]", code, i), op.Date)
			if i > 0 && op.Date < f.Operations[i-1].Date {
				errs = append(errs, fmt.Errorf("%s.operations must be sorted by date", code))
			}
		}
		for i, s := range f.Surveys {
			checkDate(fmt.Sprintf("%s.surveys[%d]", code, i), s.Date)
			if i > 0 && s.Date < f.Surveys[i-1].Date {
				errs = append(errs, fmt.Errorf("%s.surveys must be sorted by date", code))
			}
		}
		if f.Soil != nil {
			checkDate(code+".soil.sampled_at", f.Soil.SampledAt)
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
		return nil, fmt.Errorf("field mock data: %w", err)
	}
	m := &mock{data: d, clk: clk, dir: dir}
	return kit.NewServer(Name, Specs(), map[string]kit.Handler{
		ToolProfile: m.profile, ToolHistory: m.history, ToolPestSurvey: m.pestSurvey, ToolSoil: m.soil,
	})
}
