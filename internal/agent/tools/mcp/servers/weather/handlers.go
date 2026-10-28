package weather

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

type fieldArgs struct {
	kit.FieldArgs
}

type forecastArgs struct {
	kit.FieldArgs
	Days *int `json:"days"`
}

type historyArgs struct {
	kit.FieldArgs
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
}

// collectedAt 是实况/预报/预警的数据时刻：当前时刻减去观测滞后。
func (m *mock) collectedAt(now time.Time) time.Time { return now.Add(-m.data.ObservationLag) }

func (m *mock) realtime(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	if err := m.resolve(ctx, args, &a); err != nil {
		return mcp.Result{}, err
	}
	now := m.clk.Now()
	at := m.collectedAt(now)
	out := map[string]any{"field_id": a.FieldID, "observation": m.toRecord(at, m.data.Realtime)}
	return kit.NewResult(out, m.data.Source, at, now)
}

func (m *mock) forecast(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a forecastArgs
	if err := m.resolve(ctx, args, &a); err != nil {
		return mcp.Result{}, err
	}
	days := DefaultForecastDays
	if a.Days != nil {
		days = *a.Days
	}
	if days < 1 || days > MaxForecastDays {
		return mcp.Result{}, kit.Rejected("days must be in [1, %d], got %d", MaxForecastDays, days)
	}
	now := m.clk.Now()
	today := kit.DayOf(now)
	daily := make([]record, 0, days)
	for i := 0; i < days; i++ {
		day := today.AddDate(0, 0, i)
		d := m.dayAt(day)
		r := m.toRecord(day, d.Obs)
		prob := d.RainProbPct
		r.RainProbPct = &prob
		daily = append(daily, r)
	}
	at := m.collectedAt(now)
	return kit.NewResult(map[string]any{"field_id": a.FieldID, "daily": daily}, m.data.Source, at, now)
}

type alertOut struct {
	Type   string `json:"type"`
	Level  string `json:"level"`
	Title  string `json:"title"`
	Date   string `json:"date"`
	Source string `json:"source"`
}

func (m *mock) alert(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a fieldArgs
	if err := m.resolve(ctx, args, &a); err != nil {
		return mcp.Result{}, err
	}
	now := m.clk.Now()
	today := kit.DayOf(now)
	alerts := make([]alertOut, 0, AlertHorizonDays)
	for i := 0; i < AlertHorizonDays; i++ {
		day := today.AddDate(0, 0, i)
		if al := m.dayAt(day).Alert; al != nil {
			alerts = append(alerts, alertOut{Type: al.Type, Level: al.Level, Title: al.Title,
				Date: day.Format(time.DateOnly), Source: m.data.Source})
		}
	}
	at := m.collectedAt(now)
	return kit.NewResult(map[string]any{"field_id": a.FieldID, "alerts": alerts}, m.data.Source, at, now)
}

type summary struct {
	Days           int     `json:"days"`
	RainDays       int     `json:"rain_days"`
	RainMMTotal    float64 `json:"rain_mm_total"`
	TempAvgC       float64 `json:"temp_avg_c"`
	HumidityAvgPct float64 `json:"humidity_avg_pct"`
}

func (m *mock) history(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var a historyArgs
	if err := m.resolve(ctx, args, &a); err != nil {
		return mcp.Result{}, err
	}
	now := m.clk.Now()
	yesterday := kit.DayOf(now).AddDate(0, 0, -1)
	end, err := kit.ParseDate(a.EndDate, yesterday)
	if err != nil {
		return mcp.Result{}, err
	}
	start, err := kit.ParseDate(a.StartDate, end.AddDate(0, 0, 1-DefaultHistoryDays))
	if err != nil {
		return mcp.Result{}, err
	}
	if end.After(yesterday) {
		return mcp.Result{}, kit.Rejected("end_date %s must not be later than %s", a.EndDate, yesterday.Format(time.DateOnly))
	}
	n := int(end.Sub(start)/kit.Day) + 1
	if n < 1 || n > MaxHistoryDays {
		return mcp.Result{}, kit.Rejected("date range must be 1..%d days, got %d", MaxHistoryDays, n)
	}
	daily := make([]record, 0, n)
	sum := summary{Days: n}
	var temp, hum float64
	for i := 0; i < n; i++ {
		day := start.AddDate(0, 0, i)
		d := m.dayAt(day)
		daily = append(daily, m.toRecord(day, d.Obs))
		if d.RainMM >= rainDayMM {
			sum.RainDays++
		}
		sum.RainMMTotal += d.RainMM
		temp += d.TempC
		hum += d.HumidityPct
	}
	sum.RainMMTotal = round1(sum.RainMMTotal)
	sum.TempAvgC = round1(temp / float64(n))
	sum.HumidityAvgPct = round1(hum / float64(n))
	// 历史数据在结束日次日零点汇总完毕。
	at := end.Add(kit.Day)
	return kit.NewResult(map[string]any{"field_id": a.FieldID, "daily": daily, "summary": sum}, m.data.Source, at, now)
}

// round1 保留一位小数。
func round1(x float64) float64 { return math.Round(x*10) / 10 }
