package code

import (
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func outcomeSample(cat, typ string) *sample.Sample {
	s := adviceSample()
	s.Expect.Advice.Category, s.Expect.Advice.AdviceType = cat, typ
	return s
}

func TestOutcomeMatch(t *testing.T) {
	adviceCard := &model.AdviceCard{AdviceID: 7, RunID: 8, Outcome: model.OutcomeAdvice,
		Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation}
	runCases(t, OutcomeMatch{}, []gradeCase{
		{name: "advice matches ignoring ids", pass: true,
			t: &grader.Trial{Sample: outcomeSample("protect", "作业类"), Card: adviceCard}},
		{name: "category mismatch fails", evidence: `category = "protect", want "irrigate"`,
			t: &grader.Trial{Sample: outcomeSample("irrigate", "作业类"), Card: adviceCard}},
		{name: "advice_type mismatch fails", evidence: "advice_type",
			t: &grader.Trial{Sample: outcomeSample("protect", "观察类"), Card: adviceCard}},
		{name: "refused when advice expected fails", evidence: `outcome = "REFUSED"`,
			t: &grader.Trial{Sample: outcomeSample("protect", "作业类"),
				Card: &model.AdviceCard{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}}},
		{name: "refusal reason matches without card", pass: true,
			t: &grader.Trial{Sample: refusalSample(), Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}},
		{name: "refusal reason mismatch fails", evidence: "refuse_reason",
			t: &grader.Trial{Sample: refusalSample(), Outcome: model.OutcomeRefused, RefuseReason: "DATA_MISSING"}},
		{name: "no output fails", evidence: `outcome = ""`, t: &grader.Trial{Sample: refusalSample()}},
		{name: "nil sample is grader error", wantErr: true, t: &grader.Trial{}},
	})
	if g := (OutcomeMatch{}); g.Name() != "outcome_match" || g.RedLine() {
		t.Fatal("name / redline")
	}
}

func trajSample(tr sample.Trajectory) *sample.Sample {
	return &sample.Sample{ID: "t", Expect: sample.Expect{Outcome: model.OutcomeAdvice, Trajectory: tr}}
}

func TestMustCall(t *testing.T) {
	tr := sample.Trajectory{MustCall: []string{"weather.forecast", "crop.phenology"}, MustNotCall: []string{"device.*"}}
	runCases(t, MustCall{}, []gradeCase{
		{name: "superset passes", pass: true, t: &grader.Trial{Sample: trajSample(tr),
			ToolCalls: map[string]int{"weather.forecast": 2, "crop.phenology": 1, "field.profile": 1}}},
		{name: "missing required fails", evidence: "crop.phenology not called",
			t: &grader.Trial{Sample: trajSample(tr), ToolCalls: map[string]int{"weather.forecast": 1}}},
		{name: "wildcard forbidden fails", evidence: "must_not_call device.* but called device.list",
			t: &grader.Trial{Sample: trajSample(tr),
				ToolCalls: map[string]int{"weather.forecast": 1, "crop.phenology": 1, "device.list": 1}}},
		{name: "exact forbidden fails", evidence: "called device.actuate",
			t: &grader.Trial{Sample: trajSample(sample.Trajectory{MustNotCall: []string{"device.actuate"}}),
				ToolCalls: map[string]int{"device.actuate": 1}}},
		{name: "wildcard does not match other server", pass: true,
			t: &grader.Trial{Sample: trajSample(sample.Trajectory{MustNotCall: []string{"device.*"}}),
				ToolCalls: map[string]int{"devicex.list": 1}}},
		{name: "no assertions passes", pass: true, t: &grader.Trial{Sample: trajSample(sample.Trajectory{})}},
		{name: "unrecorded calls is grader error", wantErr: true, t: &grader.Trial{Sample: trajSample(tr)}},
	})
}

func TestBudget(t *testing.T) {
	b := func(steps, tokens int) *sample.Sample {
		return trajSample(sample.Trajectory{Budget: &sample.Budget{MaxSteps: steps, MaxTokens: tokens}})
	}
	runCases(t, Budget{}, []gradeCase{
		{name: "within steps", pass: true, evidence: "max_tokens not measured",
			t: &grader.Trial{Sample: b(3, 4000), ToolCalls: map[string]int{"a.b": 2, "c.d": 1}}},
		{name: "over steps fails", evidence: "steps 4 > max_steps 3",
			t: &grader.Trial{Sample: b(3, 0), ToolCalls: map[string]int{"a.b": 4}}},
		{name: "tokens only passes", pass: true, evidence: "max_tokens", t: &grader.Trial{Sample: b(0, 100)}},
		{name: "no budget passes", pass: true, t: &grader.Trial{Sample: trajSample(sample.Trajectory{})}},
		{name: "unrecorded calls is grader error", wantErr: true, t: &grader.Trial{Sample: b(3, 0)}},
	})
}
