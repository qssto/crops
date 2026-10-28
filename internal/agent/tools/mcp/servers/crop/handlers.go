package crop

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// ThresholdCrop 是阈值表适用的作物（P0 只有水稻）。
const ThresholdCrop = "水稻"

type fieldArgs struct {
	kit.FieldArgs
}

type spectrumArgs struct {
	kit.FieldArgs
	Stage string `json:"stage"`
}

type thresholdArgs struct {
	Target string `json:"target"`
	Stage  string `json:"stage"`
}

type phenologyOut struct {
	FieldID         uint64  `json:"field_id"`
	Crop            string  `json:"crop"`
	Variety         string  `json:"variety"`
	SowingDate      string  `json:"sowing_date"`
	AsOf            string  `json:"as_of"`
	DaysAfterSowing int     `json:"days_after_sowing"`
	GDDBaseC        float64 `json:"gdd_base_c"`
	GDD             float64 `json:"gdd_c_day"`
	Stage           string  `json:"stage"`
	SubStage        string  `json:"sub_stage,omitempty"`
	NextStage       string  `json:"next_stage,omitempty"`
	GDDToNext       float64 `json:"gdd_to_next,omitempty"`
}

// fieldCrop 解码入参并取地块种植信息。
func (m *mock) fieldCrop(ctx context.Context, args json.RawMessage, dst kit.FieldScoped) (FieldCrop, error) {
	code, err := kit.DecodeField(ctx, m.dir, args, dst)
	if err != nil {
		return FieldCrop{}, err
	}
	return kit.Lookup(m.data.Fields, code, "crop info")
}

// gdd 累计 [from, to) 每天的有效积温（月均温气候值 − 基温，负值按 0）。
func (m *mock) gdd(from, to time.Time) float64 {
	var sum float64
	for d := from; d.Before(to); d = d.Add(kit.Day) {
		sum += math.Max(0, m.data.MonthlyMeanTempC[d.Month()-1]-m.data.GDDBaseC)
	}
	return math.Round(sum*10) / 10
}

// phenologyAt 按播种日、积温与日期推算生育期；未播种返回 EMPTY。
func (m *mock) phenologyAt(fieldID uint64, fc FieldCrop, asOf time.Time) (phenologyOut, error) {
	sowing, err := time.Parse(time.DateOnly, fc.SowingDate)
	if err != nil {
		return phenologyOut{}, fmt.Errorf("field %d sowing_date: %w", fieldID, err)
	}
	if asOf.Before(sowing) {
		return phenologyOut{}, kit.Empty("field %d not sown until %s", fieldID, fc.SowingDate)
	}
	g := m.gdd(sowing, asOf)
	idx := 0
	for i, s := range m.data.Stages {
		if g >= s.GDDFrom {
			idx = i
		}
	}
	cur := m.data.Stages[idx]
	out := phenologyOut{FieldID: fieldID, Crop: fc.Crop, Variety: fc.Variety, SowingDate: fc.SowingDate,
		AsOf: asOf.Format(time.DateOnly), DaysAfterSowing: int(asOf.Sub(sowing) / kit.Day),
		GDDBaseC: m.data.GDDBaseC, GDD: g, Stage: cur.Stage, SubStage: cur.SubStage}
	if idx+1 < len(m.data.Stages) {
		next := m.data.Stages[idx+1]
		out.NextStage = next.SubStage
		if out.NextStage == "" {
			out.NextStage = next.Stage
		}
		out.GDDToNext = math.Round((next.GDDFrom-g)*10) / 10
	}
	return out, nil
}

func (m *mock) phenology(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	fc, err := m.fieldCrop(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	now := m.clk.Now()
	asOf := kit.DayOf(now)
	out, err := m.phenologyAt(a.FieldID, fc, asOf)
	if err != nil {
		return mcp.Result{}, err
	}
	return kit.NewResult(out, m.data.Source, asOf, now)
}

func (m *mock) variety(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	fc, err := m.fieldCrop(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	v := m.data.Varieties[fc.Variety] // validate 已保证存在
	out := map[string]any{"field_id": a.FieldID, "crop": fc.Crop, "variety": fc.Variety, "type": v.Type,
		"growth_days": v.GrowthDays, "resistance": v.Resistance, "notes": v.Notes}
	return kit.NewResult(out, m.data.Source, m.data.UpdatedAt, m.clk.Now())
}

type targetOut struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Risk   string `json:"risk"`
}

func (m *mock) pestSpectrum(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a spectrumArgs
	fc, err := m.fieldCrop(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	now := m.clk.Now()
	stage := a.Stage
	if stage == "" {
		p, err := m.phenologyAt(a.FieldID, fc, kit.DayOf(now))
		if err != nil {
			return mcp.Result{}, err
		}
		stage = p.Stage
	} else if !isStage(stage) {
		return mcp.Result{}, kit.Rejected("unknown stage %q", stage)
	}
	targets := make([]targetOut, 0, len(m.data.Spectrum))
	for _, s := range m.data.Spectrum {
		if risk, ok := s.Risk[stage]; ok {
			targets = append(targets, targetOut{Target: s.Target, Name: s.Name, Kind: s.Kind, Risk: risk})
		}
	}
	out := map[string]any{"field_id": a.FieldID, "crop": fc.Crop, "stage": stage, "targets": targets}
	return kit.NewResult(out, m.data.Source, m.data.UpdatedAt, now)
}

func (m *mock) threshold(_ context.Context, args json.RawMessage) (mcp.Result, error) {
	var a thresholdArgs
	if err := kit.DecodeArgs(args, &a); err != nil {
		return mcp.Result{}, err
	}
	if a.Target == "" || !isStage(a.Stage) {
		return mcp.Result{}, kit.Rejected("target and a known stage are required")
	}
	for _, t := range m.data.Thresholds {
		if t.Target == a.Target && slices.Contains(t.Stages, a.Stage) {
			out := map[string]any{"crop": ThresholdCrop, "target": t.Target, "stage": a.Stage, "logic": t.Logic, "indicators": t.Indicators}
			return kit.NewResult(out, m.data.Source, m.data.UpdatedAt, m.clk.Now())
		}
	}
	return mcp.Result{}, kit.Empty("no threshold for %s at %s", a.Target, a.Stage)
}

func isStage(s string) bool { return slices.Contains(kit.Stages, s) }
