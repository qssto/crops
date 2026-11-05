package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/review"
)

// writeTemp 在测试临时目录写一个文件，返回路径。
func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// objectMap 是按对象键返回内容的 ObjectGetter（缺键返回错误，同 MinIO NoSuchKey）。
type objectMap map[string][]byte

func (m objectMap) GetObject(_ context.Context, _, key string) ([]byte, error) {
	if b, ok := m[key]; ok {
		return b, nil
	}
	return nil, errors.New("NoSuchKey")
}

func calibration(t *testing.T, ready bool, reason string) []byte {
	t.Helper()
	b, err := json.Marshal(review.Report{GateReady: ready, GateReason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 只有 judges.yaml 已开、候选快照有该维度 judge 版本、且校准报告 gate_ready 的维度算「达标」；其余均未评并写明原因
// （待真实模型 / 待专家：现网三个维度均未开）。
func TestJudgeStatus(t *testing.T) {
	on := &review.Gate{Dimensions: map[string]review.GateDimItem{"fact_consistency": {Enabled: true}}}
	off := &review.Gate{Dimensions: map[string]review.GateDimItem{"fact_consistency": {Enabled: false}}}
	versioned := evalkit.Snapshot{JudgeVersions: map[string]string{"fact_consistency": "j1"}}
	key := "report/calibration/fact_consistency/j1.json"
	cases := []struct {
		name  string
		cfg   *review.Gate
		snap  evalkit.Snapshot
		objs  objectMap
		rated bool
		why   string
	}{
		{"ready", on, versioned, objectMap{key: calibration(t, true, "")}, true, ""},
		{"disabled in judges.yaml", off, versioned, objectMap{key: calibration(t, true, "")}, false, "judges.yaml"},
		{"no judge version", on, evalkit.Snapshot{}, objectMap{}, false, "judge 版本"},
		{"no calibration report", on, versioned, objectMap{}, false, "校准报告"},
		{"calibration not ready", on, versioned, objectMap{key: calibration(t, false, "kappa 下限 0.41 < 0.6")}, false, "kappa 下限"},
		{"bad report json", on, versioned, objectMap{key: []byte("{")}, false, "校准报告"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgeStatus(t.Context(), c.objs, "b", c.cfg, c.snap)["fact_consistency"]
			if got.Rated != c.rated || !strings.Contains(got.Why, c.why) {
				t.Fatalf("status = %+v, want rated=%t why~%q", got, c.rated, c.why)
			}
			if !c.rated && !strings.Contains(got.Why, "未评") {
				t.Fatalf("unrated why must say 未评: %q", got.Why)
			}
		})
	}
}

func TestLoadGateThresholds(t *testing.T) {
	th, err := loadGateThresholds(filepath.Join(repoRoot, defaultThresholds))
	if err != nil {
		t.Fatal(err)
	}
	if v := th["refusal_accuracy"]; v == nil || *v != 0 {
		t.Fatalf("refusal_accuracy = %v", v)
	}
	if v, ok := th["fact_consistency"]; !ok || v != nil {
		t.Fatalf("fact_consistency = %v %t, want null", v, ok)
	}
	bad := writeTemp(t, "thresholds.yaml", "metrics:\n  - name: x\n    unknown: 1\n")
	if _, err := loadGateThresholds(bad); err == nil {
		t.Fatal("unknown field accepted")
	}
	dup := writeTemp(t, "thresholds.yaml", "metrics:\n  - name: x\n    threshold: 0\n  - name: x\n    threshold: 1\n")
	if _, err := loadGateThresholds(dup); err == nil {
		t.Fatal("duplicate metric accepted")
	}
}
