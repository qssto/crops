package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/agent/tools/mcp/servers/device"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/seed"
)

// foreignInventory 汇总种子中非主农场的农场名、地块编码与 mock 设备 ID（样本 expect.foreign 只能取自这里）。
func foreignInventory(t *testing.T) map[string]bool {
	t.Helper()
	data, err := seed.Load(filepath.Join(repoRoot, "configs", "seed", "fields.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := device.LoadBindings(filepath.Join(repoRoot, "configs", "mock", "device.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, f := range data.ExtraFarms {
		known[f.Farm.Name] = true
		for _, fd := range f.Fields {
			known[fd.ExtFieldCode] = true
		}
		ids, err := b.DeviceIDs(context.Background(), model.Scope{TenantID: f.Farm.TenantID, FarmID: f.Farm.FarmID})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			known[id] = true
		}
	}
	return known
}

// checkForeign 要求样本声明的每个他场标识都是种子 / mock 中真实存在的他场资源，且出现在问题原文里。
func checkForeign(s *sample.Sample, known map[string]bool) error {
	for _, f := range s.Expect.Foreign {
		if !known[f] {
			return fmt.Errorf("%s: foreign %q is not a seeded non-primary farm resource", s.ID, f)
		}
		if !strings.Contains(s.Question, f) {
			return fmt.Errorf("%s: foreign %q not in question", s.ID, f)
		}
	}
	return nil
}

// TestCrossFarmSamples_UseSeededForeignFarm：cross_farm 样本的他场标识取自种子农场 (1,2) 与绑定到它的 mock 设备。
func TestCrossFarmSamples_UseSeededForeignFarm(t *testing.T) {
	known := foreignInventory(t)
	ss, _, err := sample.LoadDir(filepath.Join(repoRoot, "eval", "suites"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range ss {
		if len(s.Expect.Foreign) == 0 {
			continue
		}
		n++
		if err := checkForeign(s, known); err != nil {
			t.Error(err)
		}
	}
	if n == 0 {
		t.Fatal("no sample declares expect.foreign")
	}

	bad := []struct{ name, foreign string }{
		{"fictional field", "field-901"},
		{"own farm field", "field-001"},
		{"own farm device", "uav-p100-01"},
		{"not in question", "field-202"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			s := &sample.Sample{ID: "x", Question: "field-901 field-001 uav-p100-01",
				Expect: sample.Expect{Foreign: []string{tt.foreign}}}
			if err := checkForeign(s, known); err == nil {
				t.Fatalf("foreign %q accepted", tt.foreign)
			}
		})
	}
}
