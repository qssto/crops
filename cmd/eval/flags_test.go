package main

import (
	"errors"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/seed"
)

// P2 第 3 批：--flags 解析、FARM 行生成与快照记录；不带 --flags 时快照不变。
func TestEvalFlags(t *testing.T) {
	if f, err := parseEvalFlags(""); f != nil || err != nil {
		t.Fatalf("empty = %v, %v", f, err)
	}
	for _, bad := range []string{"enable_advice=on", "enable_compaction=shadow", "rule_offload.tank_mix=maybe",
		"rule_offload.tank_mix", "enable_compaction=on,enable_compaction=off"} {
		if _, err := parseEvalFlags(bad); !errors.Is(err, errEvalFlags) {
			t.Errorf("%q err = %v", bad, err)
		}
	}
	f, err := parseEvalFlags("rule_offload.tank_mix=shadow, enable_loader_v1=on")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := flagWrites(f, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 || ws[0].Key != model.FlagEnableLoaderV1 || string(ws[0].Value) != `{"on":true}` ||
		ws[1].Key != model.FlagRuleOffloadTankMix || string(ws[1].Value) != `{"mode":"shadow"}` ||
		ws[1].ScopeType != model.FlagScopeFarm || ws[1].TenantID != 1 || ws[1].FarmID != 2 {
		t.Fatalf("writes = %+v", ws)
	}
	base := evalkit.Snapshot{LoaderStrategy: evalkit.LoaderV0}
	if got := withFlags(base, nil); got.Flags != nil || got.LoaderStrategy != evalkit.LoaderV0 {
		t.Fatalf("no flags snapshot = %+v", got)
	}
	got := withFlags(base, f)
	if got.LoaderStrategy != evalkit.LoaderV1 || got.Flags["rule_offload.tank_mix"] != "shadow" {
		t.Fatalf("flags snapshot = %+v", got)
	}
}

func TestInfraSetFlags(t *testing.T) {
	in := &infra{seed: &seed.Data{Farm: model.FarmProfile{TenantID: 1, FarmID: 1}}}
	if err := in.setFlags(nil); err != nil || in.flags != nil || len(in.seed.Flags) != 0 {
		t.Fatalf("nil flags changed infra: %v %+v", err, in.seed.Flags)
	}
	if err := in.setFlags(map[string]string{"enable_compaction": "on"}); err != nil || len(in.seed.Flags) != 1 {
		t.Fatalf("set = %v %+v", err, in.seed.Flags)
	}
	if err := in.setFlags(map[string]string{"enable_compaction": "on"}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second set err = %v", err)
	}
	dup := &infra{seed: &seed.Data{Farm: model.FarmProfile{TenantID: 1, FarmID: 1},
		Flags: []model.FlagWrite{{TenantID: 1, FarmID: 1, Key: model.FlagEnableCompaction}}}}
	if err := dup.setFlags(map[string]string{"enable_compaction": "on"}); !errors.Is(err, errEvalFlags) {
		t.Fatalf("seed conflict err = %v", err)
	}
}
