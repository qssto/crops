package crop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agri-steward/internal/agent/tools/mcp"
	"agri-steward/internal/agent/tools/mcp/servers/kit"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var mockFile = filepath.Join("..", "..", "..", "..", "..", "..", "configs", "mock", "crop.yaml")

// TestPhenology 按播种日 + 积温 + 日期推算生育期：field-001 播种 2026-05-20，field-005 播种 2026-05-25。
func TestPhenology(t *testing.T) {
	fields := kit.StaticFields{1: "field-001", 5: "field-005"}
	tests := []struct {
		date          string
		fieldID       int
		stage, sub    string
		wantEmptyCode bool
	}{
		{date: "2026-05-10", fieldID: 1, wantEmptyCode: true}, // 未播种
		{date: "2026-05-20", fieldID: 1, stage: "秧苗期"},
		{date: "2026-06-16", fieldID: 1, stage: "返青期"},
		{date: "2026-06-25", fieldID: 1, stage: "分蘖期", sub: "分蘖初期"},
		{date: "2026-07-05", fieldID: 1, stage: "分蘖期", sub: "分蘖盛期"},
		{date: "2026-07-25", fieldID: 1, stage: "拔节孕穗期", sub: "拔节期"},
		{date: "2026-08-12", fieldID: 1, stage: "拔节孕穗期", sub: "孕穗期"},
		{date: "2026-08-27", fieldID: 1, stage: "抽穗扬花期", sub: "始穗期"},
		{date: "2026-09-15", fieldID: 1, stage: "灌浆结实期", sub: "乳熟期"},
		{date: "2026-10-20", fieldID: 1, stage: "成熟期", sub: "黄熟期"},
		{date: "2026-12-01", fieldID: 1, stage: "成熟期", sub: "完熟期"},
		{date: "2026-08-12", fieldID: 5, stage: "拔节孕穗期", sub: "幼穗分化期"}, // 晚播 5 天，发育更晚
	}
	for _, tt := range tests {
		t.Run(tt.date, func(t *testing.T) {
			at, err := time.Parse(time.DateOnly, tt.date)
			if err != nil {
				t.Fatal(err)
			}
			srv, err := LoadMock(mockFile, fixedClock{at.Add(9 * time.Hour)}, fields)
			if err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]int{"field_id": tt.fieldID})
			if err != nil {
				t.Fatal(err)
			}
			res, err := srv.Invoke(context.Background(), ToolPhenology, args)
			if tt.wantEmptyCode {
				var te *mcp.ToolError
				if !errors.As(err, &te) || te.Code != mcp.ErrEmpty {
					t.Fatalf("err = %v, want EMPTY", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var out phenologyOut
			if err := json.Unmarshal(res.Data, &out); err != nil {
				t.Fatal(err)
			}
			if out.Stage != tt.stage || out.SubStage != tt.sub {
				t.Fatalf("stage = %s/%s (gdd %.1f), want %s/%s", out.Stage, out.SubStage, out.GDD, tt.stage, tt.sub)
			}
			if res.DataFreshnessSec != int((9 * time.Hour).Seconds()) {
				t.Errorf("freshness = %d, want 9h since day start", res.DataFreshnessSec)
			}
		})
	}
}

// TestNewMock_RejectsBadData 覆盖 mock 数据校验的失败路径。
func TestNewMock_RejectsBadData(t *testing.T) {
	var good Data
	if err := kit.LoadYAML(mockFile, &good); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(d *Data)
	}{
		{"no source", func(d *Data) { d.Source = "" }},
		{"short climate table", func(d *Data) { d.MonthlyMeanTempC = d.MonthlyMeanTempC[:11] }},
		{"unknown stage", func(d *Data) { d.Stages = append(d.Stages, StageDef{Stage: "抽穗期", GDDFrom: 9999}) }},
		{"gdd not increasing", func(d *Data) { d.Stages = append(d.Stages, StageDef{Stage: "成熟期", GDDFrom: 1}) }},
		{"bad sowing date", func(d *Data) { d.Fields = map[string]FieldCrop{"x": {Variety: "南粳9108", SowingDate: "5/20"}} }},
		{"unknown variety", func(d *Data) { d.Fields = map[string]FieldCrop{"x": {Variety: "?", SowingDate: "2026-05-20"}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := good
			d.Stages = append([]StageDef(nil), good.Stages...)
			tt.mutate(&d)
			if _, err := NewMock(d, fixedClock{}, kit.StaticFields{}); err == nil {
				t.Fatal("want error")
			}
		})
	}
	if _, err := NewMock(good, fixedClock{}, kit.StaticFields{}); err != nil {
		t.Fatalf("good data: %v", err)
	}
}
