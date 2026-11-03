package report

import (
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// suiteExperiment：regression 一个样本、redteam 两个样本（其一 1 次红线失败），各跑 n 次。
func suiteExperiment(n int, suites ...string) *evalkit.Experiment {
	exp := &evalkit.Experiment{ID: "x", N: n, Suites: suites}
	add := func(suite, id string, fail int) {
		for i := range n {
			exp.Trials = append(exp.Trials, evalkit.Trial{SampleID: id, Suite: suite, TrialIndex: i,
				Scores: []evalkit.Score{{Name: "injection_ignored", RedLine: true, Pass: i != fail, Value: 1}}})
		}
		exp.Samples++
	}
	for _, s := range suites {
		add(s, s+"-a", -1)
		if s == suiteRedteam {
			add(s, s+"-b", 0)
		}
	}
	return exp
}

// TestRender_Suites（P1 L8）：多套件 / 含 redteam 时追加「按套件红线 pass^k」，redteam 单列 pass^5；
// n < 5 不判定 pass^5；单个非 redteam 套件不出该段（既有报告不变）。
func TestRender_Suites(t *testing.T) {
	tests := []struct {
		name    string
		exp     *evalkit.Experiment
		want    []string
		wantNot []string
	}{
		{name: "nightly n=5", exp: suiteExperiment(5, "regression", suiteRedteam), want: []string{
			"## 按套件红线 pass^k\n",
			"| regression | 1 | 5 | 5 | 0 | 1.00 | 1 / 1 | 1.00 | - |",
			"| redteam | 2 | 10 | 5 | 0 | 0.50 | 1 / 2 | 0.50 | - |",
			"redteam 单列：2 个样本 n 次红线全过 1 个，pass^5 = 0.50",
		}},
		{name: "n below 5 not judged", exp: suiteExperiment(3, "regression", suiteRedteam), want: []string{
			"| redteam | 2 | 6 | 3 | 0 | 0.50 | 1 / 2 | n<5 不判定 | - |",
			"pass^5 不判定",
		}},
		{name: "redteam alone", exp: suiteExperiment(5, suiteRedteam), want: []string{"| redteam | 2 | 10 |"}},
		{name: "single regression suite unchanged", exp: suiteExperiment(3, "regression"),
			wantNot: []string{"按套件红线 pass^k"}},
		// P1-L8-A 审查修复
		{name: "grader error is not judged", exp: withScore(suiteExperiment(5, "regression", suiteRedteam), suiteRedteam,
			evalkit.Score{Name: "forbidden_text", RedLine: true, Error: "grader forbidden_text: boom"}), want: []string{
			"| redteam | 2 | 10 | 5 | 0 | 不可判定（grader_error 10） | 0 / 2 | 不可判定（grader_error 10） |",
			"pass^5 不可判定（grader_error 10）",
		}, wantNot: []string{"pass^5 = "}},
		{name: "inapplicable red line listed", exp: withScore(suiteExperiment(5, "regression", suiteRedteam), "",
			evalkit.Score{Name: "forbidden_text", RedLine: true, Pass: true, Value: 1, NotApplicable: true}), want: []string{
			"| redteam | 2 | 10 | 5 | 0 | 0.50 | 1 / 2 | 0.50 | forbidden_text |",
			"redteam 中无适用样本的红线：forbidden_text",
			"| forbidden_text | 是 | 无适用样本 | 15 | 0 | 0 | 1.00 |",
		}},
		{name: "tag filtered run is a subset", exp: func() *evalkit.Experiment {
			e := suiteExperiment(5, "regression", suiteRedteam)
			e.Tags = []string{"L8"}
			return e
		}(), want: []string{"pass^5 = 0.50（夜间全量要求 N ≥ 5）；子集（tag 过滤：L8），不代表全量。"}},
		{name: "quarantined count per suite", exp: func() *evalkit.Experiment {
			e := suiteExperiment(5, "regression", suiteRedteam)
			e.Quarantined = []evalkit.QuarantinedSample{{ID: "q1", Suite: "regression", Reason: "r"}, {ID: "q2", Suite: "regression", Reason: "r"}}
			return e
		}(), want: []string{"| regression | 1 | 5 | 5 | 2 | 1.00 |", "| redteam | 2 | 10 | 5 | 0 |"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := Render(&b, tt.exp); err != nil {
				t.Fatal(err)
			}
			out := b.String()
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("report missing %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("report must not contain %q:\n%s", w, out)
				}
			}
			if gate := strings.Index(out, "## Gate 0"); gate >= 0 && strings.Index(out, "## 按套件红线") >= 0 &&
				strings.Index(out, "## 按套件红线") < gate {
				t.Errorf("suite section must come after Gate 0:\n%s", out)
			}
		})
	}
}

// withScore 给 suite（为空时全部套件）的每个 trial 追加一个打分。
func withScore(exp *evalkit.Experiment, suite string, sc evalkit.Score) *evalkit.Experiment {
	for i := range exp.Trials {
		if suite == "" || exp.Trials[i].Suite == suite {
			exp.Trials[i].Scores = append(exp.Trials[i].Scores, sc)
		}
	}
	return exp
}

// TestFmtSuiteValue（P1-L8-A 审查修复）：只有全部样本 n 次全过才写 1.00；有 grader_error 写不可判定。
func TestFmtSuiteValue(t *testing.T) {
	tests := []struct {
		name string
		st   reduce.SuiteStat
		v    float64
		want string
	}{
		{"all pass", reduce.SuiteStat{Samples: 3, AllPassSamples: 3}, 1, "1.00"},
		{"rounds to 1.00 but one sample failed", reduce.SuiteStat{Samples: 300, AllPassSamples: 299}, 0.9975, "<1.00"},
		{"below", reduce.SuiteStat{Samples: 2, AllPassSamples: 1}, 0.5, "0.50"},
		{"grader error", reduce.SuiteStat{Samples: 2, AllPassSamples: 2, GraderErrors: 1}, 1, "不可判定（grader_error 1）"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fmtSuiteValue(tt.st, tt.v); got != tt.want {
				t.Fatalf("fmtSuiteValue = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestProviderLine（P1-L8-A 审查修复）：报告抬头写模型 provider；stub 另标确定性回归。
func TestProviderLine(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{evalkit.ModelStub, "模型 provider：stub；确定性回归（stub 模型，n>1 不提供稳定性信息）"},
		{"openai", "模型 provider：openai"},
		{"", "模型 provider：未记录"},
	}
	for _, tt := range tests {
		if got := ProviderLine(evalkit.Snapshot{Model: tt.model}); got != tt.want {
			t.Errorf("ProviderLine(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
	var b strings.Builder
	exp := suiteExperiment(1, "regression")
	exp.Snapshot = evalkit.Snapshot{GitSHA: "abc", Model: evalkit.ModelStub}
	if err := Render(&b, exp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "\n\n> 模型 provider：stub；确定性回归") {
		t.Fatalf("report header missing provider line:\n%s", b.String())
	}
}
