package repository

import (
	"testing"
	"time"
)

func TestProfileRow_Summary(t *testing.T) {
	row := profileRow{FarmName: "东湖农场", Region: "jiangsu", MainCrops: `["rice"]`, FieldCode: "field-001", Crop: "rice",
		Sowing: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC), AreaMu: "35.50", Prefs: `{"pesticide":"吡蚜酮"}`}
	got, err := row.summary()
	want := `农场东湖农场（jiangsu），常种rice；地块field-001种rice，播种于2026-06-10，面积35.50亩；偏好{"pesticide":"吡蚜酮"}`
	if err != nil || got != want {
		t.Fatalf("summary = %q, %v\nwant %q", got, err, want)
	}
	row.Prefs = `{}`
	if got, err := row.summary(); err != nil || got != want[:len(want)-len(`；偏好{"pesticide":"吡蚜酮"}`)] {
		t.Fatalf("empty prefs summary = %q, %v", got, err)
	}
	row.MainCrops = `not json`
	if _, err := row.summary(); err == nil {
		t.Fatal("want main_crops decode error")
	}
}
