package field

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

// opFertilize 是施肥农事类型，history 额外返回最近一次施肥。
const opFertilize = "fertilize"

type fieldArgs struct {
	kit.FieldArgs
}

type historyArgs struct {
	kit.FieldArgs
	Kind  string `json:"kind"`
	Since string `json:"since"`
}

type surveyArgs struct {
	kit.FieldArgs
	Target string `json:"target"`
}

// fieldView 是一次调用所见的地块数据（带编码）。
type fieldView struct {
	FieldData
	code string
}

// load 解码入参并取地块 mock 数据，返回当前时刻与今天日期（YYYY-MM-DD）。
func (m *mock) load(ctx context.Context, args json.RawMessage, dst kit.FieldScoped) (fieldView, time.Time, string, error) {
	code, err := kit.DecodeField(ctx, m.dir, args, dst)
	if err != nil {
		return fieldView{}, time.Time{}, "", err
	}
	f, err := kit.Lookup(m.data.Fields, code, "field data")
	if err != nil {
		return fieldView{}, time.Time{}, "", err
	}
	now := m.clk.Now()
	return fieldView{FieldData: f, code: code}, now, kit.DayOf(now).Format(time.DateOnly), nil
}

// result 以记录日期（当天 UTC 零点）为数据时间组装返回。
func (m *mock) result(out any, date string, now time.Time) (mcp.Result, error) {
	at, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return mcp.Result{}, fmt.Errorf("field mock date %q: %w", date, err)
	}
	return kit.NewResult(out, m.data.Source, at, now)
}

type profileOut struct {
	FieldID   uint64 `json:"field_id"`
	FieldCode string `json:"field_code"`
	Profile
}

func (m *mock) profile(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	f, now, _, err := m.load(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	out := profileOut{FieldID: a.FieldID, FieldCode: f.code, Profile: f.Profile}
	return kit.NewResult(out, m.data.Source, f.Profile.UpdatedAt, now)
}

func (m *mock) history(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a historyArgs
	f, now, today, err := m.load(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	if _, err := kit.ParseDate(a.Since, time.Time{}); err != nil {
		return mcp.Result{}, err
	}
	ops := make([]Operation, 0, len(f.Operations))
	var lastFert *Operation
	for i, op := range f.Operations {
		if op.Date > today {
			break // 已按日期排序，之后的都是未来记录
		}
		if op.Kind == opFertilize {
			lastFert = &f.Operations[i]
		}
		if (a.Kind == "" || op.Kind == a.Kind) && op.Date >= a.Since {
			ops = append(ops, op)
		}
	}
	out := map[string]any{"field_id": a.FieldID, "operations": ops}
	if lastFert != nil {
		out["last_fertilize"] = lastFert
	}
	if len(ops) == 0 {
		return kit.NewResult(out, m.data.Source, now, now) // 空列表：确认截至现在无记录
	}
	return m.result(out, ops[len(ops)-1].Date, now)
}

type surveyOut struct {
	FieldID uint64 `json:"field_id"`
	Survey
}

func (m *mock) pestSurvey(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a surveyArgs
	f, now, today, err := m.load(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	for i := len(f.Surveys) - 1; i >= 0; i-- {
		s := f.Surveys[i]
		if s.Date > today {
			continue
		}
		obs := make([]Observation, 0, len(s.Observations))
		for _, o := range s.Observations {
			if a.Target == "" || o.Target == a.Target {
				obs = append(obs, o)
			}
		}
		if len(obs) == 0 {
			continue
		}
		s.Observations = obs
		return m.result(surveyOut{FieldID: a.FieldID, Survey: s}, s.Date, now)
	}
	return mcp.Result{}, kit.Empty("no pest survey for field %d", a.FieldID)
}

type soilOut struct {
	FieldID uint64 `json:"field_id"`
	Soil
}

func (m *mock) soil(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	f, now, today, err := m.load(ctx, args, &a)
	if err != nil {
		return mcp.Result{}, err
	}
	if f.Soil == nil || f.Soil.SampledAt > today {
		return mcp.Result{}, kit.Empty("no soil test for field %d", a.FieldID)
	}
	return m.result(soilOut{FieldID: a.FieldID, Soil: *f.Soil}, f.Soil.SampledAt, now)
}
