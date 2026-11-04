package review

import (
	"fmt"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/grader/judge"
	"agri-steward/internal/evalkit/store"
)

func codeScore(name string, pass bool) evalkit.Score {
	return evalkit.Score{Name: name, RedLine: true, Pass: pass, Value: b2f(pass)}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func judgeSc(dim string, v int) evalkit.Score {
	if v == 0 {
		return evalkit.Score{Name: dim, Pass: true, Unknown: true}
	}
	return evalkit.Score{Name: dim, Pass: true, Value: float64(v)}
}

func trial(id string, idx int, scores ...evalkit.Score) evalkit.Trial {
	return evalkit.Trial{SampleID: id, TrialIndex: idx, Suite: "regression", Scores: scores}
}

var versions = map[string]string{judge.DimFactConsistency: "fc-v", judge.DimConclusionMatch: "cm-v", judge.DimExplanationQuality: "eq-v"}

func reasonsOf(tasks []store.ReviewTask) map[string][]string {
	out := map[string][]string{}
	for _, t := range tasks {
		out[t.QueueReason] = append(out[t.QueueReason], fmt.Sprintf("%s#%d/%s", t.SampleID, *t.TrialIndex, t.Dimension))
	}
	return out
}

// 四类入口各自命中；CONFLICT 优先于其他入口。
func TestQueue_FourEntries(t *testing.T) {
	trials := []evalkit.Trial{
		// UNKNOWN：explanation_quality 无法判断。
		trial("s-unknown", 0, codeScore("citation_valid", true), judgeSc(judge.DimExplanationQuality, 0)),
		// CONFLICT：judge 3 分但 citation_valid 失败；judge 1 分但 citation_valid 通过。
		trial("s-conflict", 0, codeScore("citation_valid", false), judgeSc(judge.DimFactConsistency, 3)),
		trial("s-conflict", 1, codeScore("citation_valid", true), judgeSc(judge.DimFactConsistency, 1)),
		// FLAKY：红线一过一不过（同一实验各 trial 打分器相同，缺维度按未通过计）。
		trial("s-flaky", 0, codeScore("citation_valid", true)),
		trial("s-flaky", 1, codeScore("citation_valid", false)),
		// 一致：不入 CONFLICT。
		trial("s-agree", 0, codeScore("citation_valid", true), judgeSc(judge.DimFactConsistency, 2)),
		// grader_error 的 judge 不比；NotApplicable 的代码打分器不比。
		trial("s-err", 0, codeScore("citation_valid", false), evalkit.Score{Name: judge.DimFactConsistency, Error: "x"}),
		trial("s-na", 0, evalkit.Score{Name: "citation_valid", Pass: true, NotApplicable: true}, judgeSc(judge.DimFactConsistency, 1)),
		// holdout：不进人工队列。
		{SampleID: "s-holdout", Suite: "holdout", Scores: []evalkit.Score{codeScore("citation_valid", true), judgeSc(judge.DimFactConsistency, 1)}},
		// suite 写成 regression 但路径标为 holdout：同样不进（L2 评审 P1-2）。
		{SampleID: "s-holdout", TrialIndex: 1, Suite: "regression", Holdout: true, Scores: []evalkit.Score{codeScore("citation_valid", true), judgeSc(judge.DimFactConsistency, 1)}},
	}
	tasks, counts := Queue(QueueInput{ExperimentID: 9, Trials: trials, JudgeVersions: versions, Seed: 1}, Rules)
	got := reasonsOf(tasks)
	want := map[string][]string{
		store.ReasonConflict: {"s-conflict#0/fact_consistency", "s-conflict#1/fact_consistency"},
		store.ReasonUnknown:  {"s-unknown#0/explanation_quality"},
		// s-conflict 的 citation_valid 一过一不过，同样 flaky；整 trial 任务（维度空）与 CONFLICT 任务 ref_key 不同。
		store.ReasonFlaky: {"s-conflict#0/", "s-conflict#1/", "s-flaky#0/", "s-flaky#1/"},
	}
	for reason, w := range want {
		if fmt.Sprint(got[reason]) != fmt.Sprint(w) || counts[reason] != len(w) {
			t.Errorf("%s = %v (count %d), want %v", reason, got[reason], counts[reason], w)
		}
	}
	// 通过的 trial：s-unknown#0、s-conflict#1、s-flaky#0、s-agree#0、s-na#0 共 5 个 → ceil(0.5) = 1 个；
	// 抽中的若已以 FLAKY 入队（同为整 trial 任务、ref_key 相同），Queue 只留 FLAKY 一单。
	in := QueueInput{ExperimentID: 9, Trials: trials, JudgeVersions: versions, Seed: 1}
	if n := len(Random10Rule{}.Match(in)); n != 1 {
		t.Errorf("RANDOM_10 matched %d, want 1 of 5 passing trials", n)
	}
	for _, task := range tasks {
		if task.TaskType != store.TaskHumanQueue || *task.ExperimentID != 9 || task.SampleID == "s-holdout" {
			t.Errorf("task = %+v", task)
		}
		if task.Dimension != "" && task.JudgeVersion != versions[task.Dimension] {
			t.Errorf("judge_version = %q for %s", task.JudgeVersion, task.Dimension)
		}
	}
}

// 验收 3：judge 与 citation_valid 结论冲突的 trial 100% 入队（即便同时 UNKNOWN 于其他维度、或被随机抽检）。
func TestQueue_AllCitationConflictsQueued(t *testing.T) {
	var trials []evalkit.Trial
	want := map[string]bool{}
	for i := range 200 {
		js := i%3 + 1
		pass := i%2 == 0
		id := fmt.Sprintf("s-%03d", i)
		trials = append(trials, trial(id, 0, codeScore("citation_valid", pass), judgeSc(judge.DimFactConsistency, js),
			judgeSc(judge.DimExplanationQuality, i%4)))
		if (js >= 2) != pass {
			want[id] = true
		}
	}
	tasks, _ := Queue(QueueInput{ExperimentID: 1, Trials: trials, JudgeVersions: versions, Seed: 42}, Rules)
	got := map[string]bool{}
	for _, task := range tasks {
		if task.QueueReason == store.ReasonConflict && task.Dimension == judge.DimFactConsistency {
			got[task.SampleID] = true
		}
	}
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("conflicts queued %d, want %d", len(got), len(want))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("conflict %s not queued", id)
		}
	}
}

// RANDOM_10：同 seed 可复现，不同 seed 抽到不同 trial，比例向上取整。
func TestRandom10_Seeded(t *testing.T) {
	var trials []evalkit.Trial
	for i := range 95 {
		trials = append(trials, trial(fmt.Sprintf("s-%03d", i), 0, codeScore("outcome_match", true)))
	}
	pick := func(seed uint64) string {
		return fmt.Sprint(reasonsOf(Random10Rule{}.Match(QueueInput{Trials: trials, Seed: seed}))[store.ReasonRandom10])
	}
	a, b := Random10Rule{}.Match(QueueInput{Trials: trials, Seed: 3}), pick(3)
	if len(a) != 10 || pick(3) != b || pick(4) == b {
		t.Fatalf("random10: n=%d same-seed %v different-seed equal %v", len(a), pick(3) == b, pick(4) == b)
	}
}

// L2 评审裁决：门禁未开的 judge 出错（AdvisoryError）按 UNKNOWN 入人工队列，不参与 CONFLICT 比较。
func TestQueue_AdvisoryErrorAsUnknown(t *testing.T) {
	trials := []evalkit.Trial{trial("s-adv", 0, codeScore("citation_valid", false),
		evalkit.Score{Name: judge.DimFactConsistency, Pass: true, AdvisoryError: "bad evidence"})}
	tasks, _ := Queue(QueueInput{ExperimentID: 9, Trials: trials, JudgeVersions: versions, Seed: 1}, Rules)
	got := reasonsOf(tasks)
	if fmt.Sprint(got[store.ReasonUnknown]) != "[s-adv#0/fact_consistency]" || len(got[store.ReasonConflict]) != 0 {
		t.Fatalf("tasks = %v, want one UNKNOWN for fact_consistency", got)
	}
}
