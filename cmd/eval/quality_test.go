package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/report"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/model"
)

// qualityStore 是 report.MetricsStore 的内存实现（不存在返回 model.ErrNotFound）。
type qualityStore struct{ objs map[string][]byte }

func (s *qualityStore) PutObject(_ context.Context, _, key string, b []byte, _ string) error {
	s.objs[key] = b
	return nil
}

func (s *qualityStore) GetObject(_ context.Context, _, key string) ([]byte, error) {
	b, ok := s.objs[key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func qualityExperiment(graderErr bool, gate string) *evalkit.Experiment {
	sc := evalkit.Score{Name: "citation_valid", RedLine: true, Pass: true}
	if graderErr {
		sc = evalkit.Score{Name: "citation_valid", Error: "boom"}
	}
	return &evalkit.Experiment{ID: "9", Gate: evalkit.GateStatus{Status: gate, VoidReason: map[string]string{store.StatusVoid: "infra_error_rate"}[gate]},
		Trials: []evalkit.Trial{{SampleID: "s1", SampleVersion: 1, RunID: 77, Scores: []evalkit.Score{sc}}}}
}

// 只有 nightly 写 metrics.json / 下钻页；其他触发方式不写任何对象（默认行为不变）。grader_error 或门禁终态非 DONE
// （P2-L3 修复 P1-2：VOID / BUDGET_EXCEEDED / 未判定）不写 metrics.json，只写下钻页与作废的 latest 指针。
func TestNightlyQuality(t *testing.T) {
	ls := &loadedSamples{}
	drill := []string{report.LatestNightlyKey, "9/report.html"}
	full := append([]string{"9/metrics.json"}, drill...)
	tests := []struct {
		name      string
		trigger   string
		graderErr bool
		gate      string
		want      []string
		void      string
	}{
		{"manual untouched", store.TriggerManual, false, gateDone, nil, ""},
		{"pr untouched", store.TriggerPR, false, gateDone, nil, ""},
		{"nightly", store.TriggerNightly, false, gateDone, full, ""},
		{"nightly grader_error void", store.TriggerNightly, true, gateDone, drill, report.VoidGraderError},
		{"nightly gate VOID", store.TriggerNightly, false, store.StatusVoid, drill, "VOID:infra_error_rate"},
		{"nightly BUDGET_EXCEEDED", store.TriggerNightly, false, store.StatusBudgetExceeded, drill, store.StatusBudgetExceeded},
		{"nightly gate unsettled", store.TriggerNightly, false, "", drill, gateUnsettled},
	}
	for _, tt := range tests {
		obj := &qualityStore{objs: map[string][]byte{}}
		exp := qualityExperiment(tt.graderErr, tt.gate)
		if err := nightlyQuality(t.Context(), obj, "b", tt.trigger, ls, &judgeFlags{}, exp, exp, io.Discard); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if len(obj.objs) != len(tt.want) {
			t.Fatalf("%s: objects = %v, want %v", tt.name, obj.objs, tt.want)
		}
		for _, k := range tt.want {
			if _, ok := obj.objs[k]; !ok {
				t.Errorf("%s: missing %s", tt.name, k)
			}
		}
		if tt.want == nil {
			continue
		}
		var latest model.LatestNightly
		if err := json.Unmarshal(obj.objs[report.LatestNightlyKey], &latest); err != nil {
			t.Fatalf("%s: latest: %v", tt.name, err)
		}
		if latest.Void != (tt.void != "") || latest.VoidReason != tt.void {
			t.Errorf("%s: latest = %+v, want void %q", tt.name, latest, tt.void)
		}
	}
}
