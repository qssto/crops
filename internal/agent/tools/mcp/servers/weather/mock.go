package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
	"agri-steward/internal/clock"
)

// rainDayMM 是计入雨日的日降雨下限。
const rainDayMM = 0.1

// Data 是 configs/mock/weather.yaml：示范农场共用一个站点，日序列按日期循环投影到任意日期（结果确定）。
type Data struct {
	Source         string        `yaml:"source"`
	ObservationLag time.Duration `yaml:"observation_lag"` // 实况/预报相对当前时刻的滞后，决定 DataFreshnessSec
	Fields         []string      `yaml:"fields"`          // 有气象数据的地块编码
	Realtime       Obs           `yaml:"realtime"`
	Daily          []DayData     `yaml:"daily"`
}

// Obs 是一组气象要素。
type Obs struct {
	TempC       float64 `yaml:"temp_c"`
	HumidityPct float64 `yaml:"humidity_pct"`
	WindSpeedMS float64 `yaml:"wind_speed_ms"`
	RainMM      float64 `yaml:"rain_mm"`
}

// DayData 是循环日序列中的一天，可带一条预警。
type DayData struct {
	Obs         `yaml:",inline"`
	RainProbPct float64    `yaml:"rain_prob_pct"`
	Alert       *AlertData `yaml:"alert,omitempty"`
}

// AlertData 是一条预警。
type AlertData struct {
	Type  string `yaml:"type"`
	Level string `yaml:"level"`
	Title string `yaml:"title"`
}

func (d Data) validate() error {
	var errs []error
	if d.Source == "" {
		errs = append(errs, errors.New("source is required"))
	}
	if d.ObservationLag < 0 {
		errs = append(errs, errors.New("observation_lag must be >= 0"))
	}
	if len(d.Fields) == 0 || len(d.Daily) == 0 {
		errs = append(errs, errors.New("fields and daily must be non-empty"))
	}
	return errors.Join(errs...)
}

// record 是统一气象记录（输出）。
type record struct {
	Time        string   `json:"time"`
	TempC       float64  `json:"temp_c"`
	HumidityPct float64  `json:"humidity_pct"`
	WindSpeedMS float64  `json:"wind_speed_ms"`
	RainMM      float64  `json:"rain_mm"`
	RainProbPct *float64 `json:"rain_prob_pct,omitempty"`
	Source      string   `json:"source"`
}

type mock struct {
	data   Data
	clk    clock.Clock
	dir    kit.FieldDirectory
	fields map[string]bool
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
		return nil, fmt.Errorf("weather mock data: %w", err)
	}
	m := &mock{data: d, clk: clk, dir: dir, fields: make(map[string]bool, len(d.Fields))}
	for _, f := range d.Fields {
		m.fields[f] = true
	}
	return kit.NewServer(Name, Specs(), map[string]kit.Handler{
		ToolRealtime: m.realtime, ToolForecast: m.forecast, ToolAlert: m.alert, ToolHistory: m.history,
	})
}

// resolve 解码入参并确认地块有气象数据。
func (m *mock) resolve(ctx context.Context, args json.RawMessage, dst kit.FieldScoped) error {
	code, err := kit.DecodeField(ctx, m.dir, args, dst)
	if err != nil {
		return err
	}
	if !m.fields[code] {
		return kit.Empty("no weather data for %s", code)
	}
	return nil
}

// dayAt 把日期投影到循环日序列。
func (m *mock) dayAt(day time.Time) DayData {
	n := int64(len(m.data.Daily))
	idx := (day.Unix()/int64(kit.Day/time.Second))%n + n
	return m.data.Daily[idx%n]
}

func (m *mock) toRecord(at time.Time, o Obs) record {
	return record{Time: at.UTC().Format(time.RFC3339), TempC: o.TempC, HumidityPct: o.HumidityPct,
		WindSpeedMS: o.WindSpeedMS, RainMM: o.RainMM, Source: m.data.Source}
}
