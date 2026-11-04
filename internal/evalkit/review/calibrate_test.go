package review

import (
	"fmt"
	"testing"
	"time"

	"agri-steward/internal/evalkit/store"
)

func cands(n int, score func(i int) int) []Candidate {
	var out []Candidate
	for i := range n {
		c := Candidate{SampleID: fmt.Sprintf("s-%03d", i), Suite: []string{"regression", "capability", "constraint"}[i%3]}
		if s := score(i); s > 0 {
			c.JudgeScore = &s
		}
		out = append(out, c)
	}
	return out
}

func keys(ts []store.ReviewTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Key())
	}
	return out
}

func TestPlanCalibration_Stratified(t *testing.T) {
	// 300 个候选：1 分 30、2 分 60、3 分 200、UNKNOWN 10。
	cs := cands(300, func(i int) int {
		switch {
		case i < 30:
			return 1
		case i < 90:
			return 2
		case i < 290:
			return 3
		}
		return 0
	})
	p := PlanCalibration(7, "fact_consistency", "v1", cs, SampleOptions{Seed: 5})
	if p.Primary != 100 || p.Second != 25 || len(p.Tasks) != 125 || len(p.Short) != 0 {
		t.Fatalf("plan primary %d second %d tasks %d short %v", p.Primary, p.Second, len(p.Tasks), p.Short)
	}
	for _, k := range []string{"1", "2", "3"} {
		if p.Strata[k] < DefaultPerStratum {
			t.Errorf("stratum %s = %d", k, p.Strata[k])
		}
	}
	primary := map[string]bool{}
	for _, task := range p.Tasks {
		if task.TaskType != store.TaskCalibration || task.Dimension != "fact_consistency" || task.JudgeVersion != "v1" {
			t.Fatalf("task = %+v", task)
		}
		if task.RaterNo == store.RaterPrimary {
			primary[task.SampleID] = true
		}
	}
	for _, task := range p.Tasks {
		if task.RaterNo == store.RaterSecond && !primary[task.SampleID] {
			t.Fatalf("second-rater task without primary: %s", task.SampleID)
		}
	}
	if again := PlanCalibration(7, "fact_consistency", "v1", cs, SampleOptions{Seed: 5}); fmt.Sprint(keys(again.Tasks)) != fmt.Sprint(keys(p.Tasks)) {
		t.Fatal("same seed gives a different plan")
	}
	if other := PlanCalibration(7, "fact_consistency", "v1", cs, SampleOptions{Seed: 6}); fmt.Sprint(keys(other.Tasks)) == fmt.Sprint(keys(p.Tasks)) {
		t.Fatal("different seed gives the same plan")
	}
}

// 层内按套件轮转：只有一层时三个套件各占约 1/3，不只抽 regression。
func TestPlanCalibration_SuitesInterleaved(t *testing.T) {
	cs := cands(300, func(int) int { return 3 })
	p := PlanCalibration(1, "conclusion_match", "v", cs, SampleOptions{Seed: 9})
	bySuite := map[string]int{}
	for _, task := range p.Tasks[:p.Primary] {
		for _, c := range cs {
			if c.SampleID == task.SampleID {
				bySuite[c.Suite]++
			}
		}
	}
	for s, n := range bySuite {
		if n < 33 || n > 34 {
			t.Errorf("suite %s picked %d of 100", s, n)
		}
	}
	if len(p.Short) != 2 {
		t.Fatalf("missing strata 1/2 should be reported: %v", p.Short)
	}
}

func TestPlanCalibration_NotEnough(t *testing.T) {
	p := PlanCalibration(1, "conclusion_match", "v", cands(10, func(i int) int { return i%3 + 1 }), SampleOptions{Seed: 1})
	if p.Primary != 10 || p.Second != 3 || len(p.Short) != 4 {
		t.Fatalf("plan = %+v", p)
	}
}

// 月度复核：用注入时钟测月份边界（北京时间切月）。
func TestMonthWindow_Boundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	tests := []struct {
		now      time.Time
		from, to string
		tag      string
	}{
		// UTC 10-31 16:30 已是北京时间 11-01 00:30：属 11 月。
		{now: time.Date(2026, 10, 31, 16, 30, 0, 0, time.UTC), from: "2026-10-31T16:00:00Z", to: "2026-11-30T16:00:00Z", tag: "2026-11"},
		// UTC 10-31 15:59:59 仍是北京时间 10-31 23:59:59：属 10 月。
		{now: time.Date(2026, 10, 31, 15, 59, 59, 0, time.UTC), from: "2026-09-30T16:00:00Z", to: "2026-10-31T16:00:00Z", tag: "2026-10"},
		// 跨年。
		{now: time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC), from: "2026-12-31T16:00:00Z", to: "2027-01-31T16:00:00Z", tag: "2027-01"},
	}
	for _, tt := range tests {
		from, to := MonthWindow(tt.now, cst)
		if from.Format(time.RFC3339) != tt.from || to.Format(time.RFC3339) != tt.to || MonthTag(tt.now, cst) != tt.tag {
			t.Errorf("%s: window [%s, %s) tag %s", tt.now, from.Format(time.RFC3339), to.Format(time.RFC3339), MonthTag(tt.now, cst))
		}
	}
	if MonthSeed(tests[0].now, cst) != 202611 || MonthSeed(tests[1].now, cst) != 202610 {
		t.Fatal("month seed")
	}
}

func TestPlanRecheck(t *testing.T) {
	cs := cands(50, func(i int) int { return i%3 + 1 })
	tasks := PlanRecheck(3, "explanation_quality", "v", cs, 0, 202611)
	if len(tasks) != DefaultRecheckSize {
		t.Fatalf("tasks = %d", len(tasks))
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.TaskType != store.TaskMonthlyRecheck || task.RaterNo != store.RaterPrimary || seen[task.SampleID] {
			t.Fatalf("task = %+v", task)
		}
		seen[task.SampleID] = true
	}
	if n := len(PlanRecheck(3, "explanation_quality", "v", cs[:5], 20, 1)); n != 5 {
		t.Fatalf("short candidates = %d", n)
	}
}
