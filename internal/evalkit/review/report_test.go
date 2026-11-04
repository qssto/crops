package review

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit/store"
)

func TestLoadGate_Repo(t *testing.T) {
	g, err := LoadGate(filepath.Join("..", "..", "..", DefaultGate))
	if err != nil {
		t.Fatal(err)
	}
	if g.KappaThreshold != 0.6 {
		t.Fatalf("threshold = %v", g.KappaThreshold)
	}
	for d, item := range g.Dimensions {
		if item.Enabled {
			t.Errorf("%s enabled before calibration with real experts", d)
		}
	}
}

func TestLoadGate_Rejects(t *testing.T) {
	for name, body := range map[string]string{
		"missing dim":   "kappa_threshold: 0.6\ndimensions: {fact_consistency: {enabled: false}}\n",
		"bad threshold": "kappa_threshold: 0\ndimensions: {fact_consistency: {}, conclusion_match: {}, explanation_quality: {}}\n",
		"unknown dim":   "kappa_threshold: 0.6\ndimensions: {fact_consistency: {}, conclusion_match: {}, explanation_quality: {}, x: {}}\n",
	} {
		p := filepath.Join(t.TempDir(), "g.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGate(p); !errors.Is(err, ErrBadGate) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func gate() *Gate {
	return &Gate{KappaThreshold: 0.6, Dimensions: map[string]GateDimItem{"fact_consistency": {}}}
}

func TestBuildReport(t *testing.T) {
	// 对角为主、每层 40（n=120 ≥ MinCalibrationPairs）：κ 高，区间下限过 0.6 → 可进门禁。
	agree := []store.ScorePair{{A: 1, B: 1, Count: 40}, {A: 2, B: 2, Count: 38}, {A: 2, B: 3, Count: 2}, {A: 3, B: 3, Count: 40}}
	r := BuildReport(ReportInput{TaskType: store.TaskCalibration, Dimension: "fact_consistency", JudgeVersion: "v",
		JudgeExpert: agree, InterExpert: agree[:1], UnknownRatio: 0.05, UnknownOK: true, Gate: gate(),
		Resamples: DefaultResamples, Seed: 1, Now: time.Unix(0, 0).UTC(), Notes: []string{"待专家"}})
	if !r.GateReady || r.Enabled || r.JudgeExpert.Low < 0.6 || r.ExpertStrata != [3]int{40, 38, 42} || len(r.Short) != 0 {
		t.Fatalf("report = %+v", r)
	}
	if r.InterExpert.OK {
		t.Fatalf("inter-expert with one category should be undefined: %+v", r.InterExpert)
	}
	var b strings.Builder
	if err := r.Markdown(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"可进门禁", "UNKNOWN 占比（主标，不进 Kappa）: 5.0%", "待实测", "待专家", "| 2 | 0 | 38 | 2 |"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("markdown missing %q:\n%s", want, b.String())
		}
	}
	// 随机分数：κ ≈ 0，未达标；层数不足列出。
	noisy := []store.ScorePair{{A: 1, B: 3, Count: 5}, {A: 3, B: 1, Count: 5}, {A: 2, B: 2, Count: 2}}
	r = BuildReport(ReportInput{Dimension: "fact_consistency", JudgeExpert: noisy, Gate: gate(), Resamples: 200, Seed: 1})
	if r.GateReady || len(r.Short) != 3 || r.UnknownRatio != nil {
		t.Fatalf("noisy report = %+v", r)
	}
}

// L2 评审 P1-3：可进门禁只给 CALIBRATION、n ≥ MinCalibrationPairs、区间可用且下限过阈值；月度复核只报一致性。
func TestBuildReport_GateReadyConditions(t *testing.T) {
	scaled := func(k int) []store.ScorePair { // 对角为主，n = 30k
		return []store.ScorePair{{A: 1, B: 1, Count: 10 * k}, {A: 2, B: 2, Count: 9 * k}, {A: 2, B: 3, Count: k}, {A: 3, B: 3, Count: 10 * k}}
	}
	cases := []struct {
		name     string
		taskType string
		pairs    []store.ScorePair
		want     bool
	}{
		{"calibration n=120", store.TaskCalibration, scaled(4), true},
		{"calibration n=90 below minimum", store.TaskCalibration, scaled(3), false},
		{"monthly recheck never gate-ready", store.TaskMonthlyRecheck, scaled(4), false},
		{"empty task type", "", scaled(4), false},
		{"undefined resamples over ratio", store.TaskCalibration, []store.ScorePair{{A: 1, B: 1, Count: 118}, {A: 2, B: 2, Count: 1}, {A: 3, B: 3, Count: 1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := BuildReport(ReportInput{TaskType: c.taskType, Dimension: "fact_consistency", JudgeVersion: "v", JudgeExpert: c.pairs,
				Gate: gate(), Resamples: DefaultResamples, Seed: 3, Now: time.Unix(0, 0).UTC()})
			if r.GateReady != c.want {
				t.Fatalf("GateReady = %v, want %v (n=%d undefined=%d usable=%v reason=%q)", r.GateReady, c.want,
					r.JudgeExpert.N, r.JudgeExpert.Undefined, r.JudgeExpert.Usable, r.GateReason)
			}
			if !c.want && r.GateReason == "" {
				t.Fatal("no gate reason for a not-ready report")
			}
			var b strings.Builder
			if err := r.Markdown(&b); err != nil {
				t.Fatal(err)
			}
			md := b.String()
			if !strings.Contains(md, "n=") || !strings.Contains(md, "无定义重抽") {
				t.Fatalf("markdown lacks n / undefined count:\n%s", md)
			}
			if c.taskType == store.TaskMonthlyRecheck && (strings.Contains(md, "可进门禁") || !strings.Contains(md, "只报一致性")) {
				t.Fatalf("recheck markdown:\n%s", md)
			}
		})
	}
}
