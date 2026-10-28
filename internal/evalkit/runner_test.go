package evalkit

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
)

// fakeClock 每次调用前进 step，用来得到确定的延迟。
type fakeClock struct {
	t    time.Time
	step time.Duration
}

func (c *fakeClock) now() time.Time {
	c.t = c.t.Add(c.step)
	return c.t
}

// scriptedAgent 对指定样本问题返回错误，其余返回 ADVICE。
type scriptedAgent struct {
	failOn string
	calls  int
}

func (a *scriptedAgent) Ask(_ context.Context, q agent.Question) (agent.Answer, error) {
	a.calls++
	if q.Text == a.failOn {
		return agent.Answer{}, errors.New("boom")
	}
	return agent.Answer{Outcome: agent.OutcomeAdvice}, nil
}

func mkSample(id, suite string, tags ...string) *sample.Sample {
	return &sample.Sample{ID: id, Suite: suite, Tags: tags, Question: "q-" + id,
		Expect: sample.Expect{Outcome: agent.OutcomeRefused}}
}

func mustRunner(t *testing.T, a agent.Agent, now func() time.Time) *Runner {
	t.Helper()
	built, err := app.Build(app.Deps{Agent: a})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(built, now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunner_Run(t *testing.T) {
	samples := []*sample.Sample{mkSample("a", "regression", "L4"), mkSample("b", "regression", "L2")}
	ag := &scriptedAgent{failOn: "q-b"}
	clock := &fakeClock{t: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), step: time.Millisecond}
	r := mustRunner(t, ag, clock.now)

	exp, err := r.Run(context.Background(), samples, Options{ExperimentID: "exp", N: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(exp.Trials), 6; got != want {
		t.Fatalf("trials = %d, want %d", got, want)
	}
	if ag.calls != 6 {
		t.Fatalf("agent calls = %d, want 6", ag.calls)
	}
	for i, tr := range exp.Trials {
		wantID := samples[i/3].ID
		if tr.SampleID != wantID || tr.TrialIndex != i%3 {
			t.Errorf("trial %d = %s#%d, want %s#%d", i, tr.SampleID, tr.TrialIndex, wantID, i%3)
		}
		if tr.Latency != time.Millisecond {
			t.Errorf("trial %d latency = %v, want 1ms", i, tr.Latency)
		}
		if tr.Expected != agent.OutcomeRefused {
			t.Errorf("trial %d expected = %s", i, tr.Expected)
		}
		switch tr.SampleID {
		case "a":
			if tr.Outcome != agent.OutcomeAdvice || tr.Error != "" {
				t.Errorf("trial %d = %+v, want ADVICE without error", i, tr)
			}
		case "b":
			if tr.Outcome != "" || !strings.Contains(tr.Error, "trial b#") || !strings.Contains(tr.Error, "boom") {
				t.Errorf("trial %d = %+v, want wrapped error", i, tr)
			}
		}
	}
}

func TestRunner_Run_Invalid(t *testing.T) {
	r := mustRunner(t, RefuseAllAgent{}, nil)
	tests := []struct {
		name string
		ctx  func() context.Context
		n    int
	}{
		{name: "n < 1", ctx: context.Background, n: 0},
		{name: "canceled ctx", n: 1, ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Run(tt.ctx(), []*sample.Sample{mkSample("a", "regression")}, Options{N: tt.n}); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestNewRunner_NilApp(t *testing.T) {
	if _, err := NewRunner(nil, nil); err == nil {
		t.Fatal("want error")
	}
}

func TestRefuseAllAgent(t *testing.T) {
	ans, err := RefuseAllAgent{}.Ask(context.Background(), agent.Question{Text: "x"})
	if err != nil || ans.Outcome != agent.OutcomeRefused || ans.RefuseReason != RefuseReasonStub {
		t.Fatalf("got %+v, %v", ans, err)
	}
}

func TestSelect(t *testing.T) {
	all := []*sample.Sample{
		mkSample("a", "regression", "L2", "protect.timing"),
		mkSample("b", "regression", "L4"),
		mkSample("c", "capability", "L2"),
	}
	tests := []struct {
		name   string
		suites []string
		tags   []string
		want   string
	}{
		{name: "no filter", want: "a,b,c"},
		{name: "suite only", suites: []string{"regression"}, want: "a,b"},
		{name: "tag L2", suites: []string{"regression"}, tags: []string{"L2"}, want: "a"},
		{name: "any of tags", tags: []string{"L4", "protect.timing"}, want: "a,b"},
		{name: "no match", tags: []string{"L8"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ids []string
			for _, s := range Select(all, tt.suites, tt.tags) {
				ids = append(ids, s.ID)
			}
			if got := strings.Join(ids, ","); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNewExperimentID(t *testing.T) {
	now := time.Date(2026, 10, 28, 3, 15, 0, 0, time.FixedZone("CST", 8*3600))
	id, err := NewExperimentID(now)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^20261027T191500-[0-9a-f]{6}$`).MatchString(id) {
		t.Fatalf("id = %s", id)
	}
	other, err := NewExperimentID(now)
	if err != nil {
		t.Fatal(err)
	}
	if other == id {
		t.Fatalf("two ids collided: %s", id)
	}
}
