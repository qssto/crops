package env

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/model"
)

func TestFixtureMemory(t *testing.T) {
	raw := map[string]any{
		"profile": map[string]any{"preferred_products": []any{"吡蚜酮"}, "area_mu": 12},
		"episodic": []any{
			map[string]any{"kind": "ADVICE", "summary": "上次建议吡蚜酮", "occurred_at": "2026-07-01T09:00:00+08:00"},
			map[string]any{"kind": "REJECTED", "summary": "作业单驳回：雨前不宜喷药",
				"occurred_at": time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)},
		},
	}
	src, err := NewFixtureMemory(raw)
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New(src, memory.Options{Enabled: true})
	items, err := st.Recent(context.Background(), model.Scope{TenantID: 1, FarmID: 1}, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Summary)
	}
	want := "PROFILE:area_mu: 12；preferred_products: [吡蚜酮]|REJECTED:作业单驳回：雨前不宜喷药|ADVICE:上次建议吡蚜酮"
	if strings.Join(got, "|") != want {
		t.Fatalf("items = %q\nwant %q", strings.Join(got, "|"), want)
	}
	one, err := src.Episodic(context.Background(), model.Scope{}, 3, 1)
	if err != nil || len(one) != 1 || one[0].Kind != "REJECTED" {
		t.Fatalf("limit 1 = %+v, %v", one, err)
	}
}

func TestFixtureMemory_Empty(t *testing.T) {
	src, err := NewFixtureMemory(nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := memory.New(src, memory.Options{Enabled: true}).Recent(context.Background(), model.Scope{}, 3, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %+v, %v", items, err)
	}
}

func TestFixtureMemory_Bad(t *testing.T) {
	for name, raw := range map[string]map[string]any{
		"unknown key":       {"working": "x"},
		"profile not map":   {"profile": "x"},
		"episodic not list": {"episodic": map[string]any{}},
		"missing summary":   {"episodic": []any{map[string]any{"kind": "ADVICE", "occurred_at": "2026-07-01T09:00:00Z"}}},
		"bad time":          {"episodic": []any{map[string]any{"kind": "ADVICE", "summary": "s", "occurred_at": "yesterday"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewFixtureMemory(raw); !errors.Is(err, errBadMemoryFixture) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
