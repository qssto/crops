package calc

import (
	"slices"
	"testing"
	"time"
)

// TestOperationWindow：任一硬约束不满足即关闭；没有判定过的约束视为未判定（不给窗口结论）。
func TestOperationWindow(t *testing.T) {
	from := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	tests := []struct {
		name            string
		checks          []WindowCheck
		evaluated, open bool
		blocked         []string
	}{
		{name: "none judged", checks: nil},
		{name: "all met", checks: []WindowCheck{{ID: "wind-knapsack", Met: true}, {ID: "rain-24h", Met: true}}, evaluated: true, open: true},
		{name: "rain blocks", checks: []WindowCheck{{ID: "wind-knapsack", Met: true}, {ID: "rain-24h"}}, evaluated: true, blocked: []string{"rain-24h"}},
		{name: "two block sorted", checks: []WindowCheck{{ID: "temp-max"}, {ID: "rain-24h"}}, evaluated: true, blocked: []string{"rain-24h", "temp-max"}},
		{name: "duplicate id kept once", checks: []WindowCheck{{ID: "rain-24h"}, {ID: "rain-24h"}}, evaluated: true, blocked: []string{"rain-24h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := OperationWindow(tt.checks, from, to)
			if w.Evaluated != tt.evaluated || w.Open != tt.open || !slices.Equal(w.Blocked, tt.blocked) {
				t.Fatalf("got %+v", w)
			}
			if w.Open && (!w.From.Equal(from) || !w.To.Equal(to)) {
				t.Fatalf("open window bounds %v-%v", w.From, w.To)
			}
		})
	}
}

// TestWindow_AdmitsDay：按本地日历日判断「今天 / 明天 / 后天」是否落在窗口内；关闭的窗口不接纳任何一天。
func TestWindow_AdmitsDay(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 7, 15, 9, 0, 0, 0, loc) // 本地 7-15 09:00
	open := OperationWindow([]WindowCheck{{ID: "rain-24h", Met: true}}, from, from.Add(24*time.Hour))
	for offset, want := range map[int]bool{-1: false, 0: true, 1: true, 2: false, 7: false} {
		if got := open.AdmitsDay(offset, loc); got != want {
			t.Errorf("open.AdmitsDay(%d) = %v, want %v", offset, got, want)
		}
	}
	short := OperationWindow([]WindowCheck{{ID: "rain-24h", Met: true}}, from, from.Add(6*time.Hour))
	if short.AdmitsDay(1, loc) {
		t.Error("6h window admits tomorrow")
	}
	closed := OperationWindow([]WindowCheck{{ID: "rain-24h"}}, from, from.Add(24*time.Hour))
	if closed.AdmitsDay(0, loc) {
		t.Error("closed window admits today")
	}
	if !closed.Covers(1, loc) || closed.Covers(2, loc) {
		t.Error("closed 24h window should cover today and tomorrow only")
	}
}
