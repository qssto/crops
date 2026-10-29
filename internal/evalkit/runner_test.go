package evalkit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
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
	return agent.Answer{Outcome: model.OutcomeAdvice}, nil
}

func mkSample(id, suite string, tags ...string) *sample.Sample {
	return &sample.Sample{ID: id, Suite: suite, Tags: tags, Question: "q-" + id,
		Expect: sample.Expect{Outcome: model.OutcomeRefused}}
}

func mustRunner(t *testing.T, a agent.Agent, now func() time.Time) *Runner {
	t.Helper()
	r, err := NewRunner(StaticEnv(&app.App{Agent: a}), now)
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
		if tr.Expected != model.OutcomeRefused {
			t.Errorf("trial %d expected = %s", i, tr.Expected)
		}
		switch tr.SampleID {
		case "a":
			if tr.Outcome != model.OutcomeAdvice || tr.Error != "" {
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

// toolAgent 依次调用给定工具后拒答，用来验证 trial 记录 FixtureMCP 的调用。
type toolAgent struct {
	tools agent.ToolProvider
	calls []string
}

func (a toolAgent) Ask(ctx context.Context, _ agent.Question) (agent.Answer, error) {
	for _, tool := range a.calls {
		if _, err := a.tools.Call(ctx, model.Scope{}, agent.ToolCall{Tool: tool}); err != nil {
			return agent.Answer{}, err
		}
	}
	return agent.Answer{Outcome: model.OutcomeRefused, RefuseReason: "DATA_MISSING"}, nil
}

func TestRunner_RecordsToolCalls(t *testing.T) {
	fx := map[string][]sample.ToolFixture{"weather.forecast": {{Error: sample.FaultTimeout}}}
	f, err := env.NewFixtureMCP(context.Background(), fx, nil, env.NewFrozenClock(time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	ag := toolAgent{tools: f, calls: []string{"weather.forecast", "device.list"}}
	r, err := NewRunner(StaticEnv(&app.App{Agent: ag, Tools: f}), nil)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.Run(context.Background(), []*sample.Sample{mkSample("a", "regression")}, Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	tr := exp.Trials[0]
	if tr.ToolCalls["weather.forecast"] != 1 || tr.ToolCalls["device.list"] != 1 ||
		strings.Join(tr.UnexpectedCalls, ",") != "device.list" {
		t.Fatalf("trial = %+v", tr)
	}
	if got := strings.Join(tr.ToolFailures, ","); got != "weather.forecast:"+env.StatusTimeout {
		t.Fatalf("tool failures = %q", got)
	}
	// 未接 RecordingAdapter / LogCapture / DB：对应观测保持 nil（打分器据此判 grader_error 而非通过）。
	if tr.DeviceCalls != nil || tr.Logs != nil || tr.Approval != nil || tr.Stored != nil {
		t.Fatalf("unrecorded observations must stay nil: %+v", tr)
	}
}

// deviceAgent 写一条日志并调一次设备 DryRun 后拒答，用来验证 trial 记录 Adapter 调用与捕获日志。
type deviceAgent struct {
	dev    adapter.DeviceAdapter
	logger *slog.Logger
}

func (a deviceAgent) Ask(ctx context.Context, _ agent.Question) (agent.Answer, error) {
	a.logger.Info("ask", "q", "电话13800001234")
	if _, err := a.dev.DryRun(ctx, model.WorkOrder{OrderID: 5}); err != nil {
		return agent.Answer{}, err
	}
	return agent.Answer{Outcome: model.OutcomeRefused, RefuseReason: "DATA_MISSING"}, nil
}

func TestRunner_RecordsDeviceCallsAndLogs(t *testing.T) {
	dev := &env.RecordingAdapter{}
	logger, err := app.NewTappedLogger(io.Discard, "info", env.NewLogCapture().Tap)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(StaticEnv(&app.App{Agent: deviceAgent{dev: dev, logger: logger}, Adapter: dev, Logger: logger}), nil)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.Run(context.Background(), []*sample.Sample{mkSample("a", "regression")}, Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	tr := exp.Trials[0]
	if strings.Join(tr.DeviceCalls, ",") != "DryRun order=5" {
		t.Fatalf("device calls = %v", tr.DeviceCalls)
	}
	// 捕获的是脱敏层之后实际写出的内容。
	if logs := string(tr.Logs); !strings.Contains(logs, "138****1234") || strings.Contains(logs, "13800001234") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestRunner_EnvError(t *testing.T) {
	failing := func(context.Context, *sample.Sample, int) (*app.App, func() error, error) {
		return nil, nil, errors.New("schema pool exhausted")
	}
	r, err := NewRunner(failing, nil)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := r.Run(context.Background(), []*sample.Sample{mkSample("a", "regression")}, Options{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if tr := exp.Trials[0]; !strings.Contains(tr.Error, "env: schema pool exhausted") {
		t.Fatalf("trial = %+v", tr)
	}
}

func TestNewRunner_NilApp(t *testing.T) {
	if _, err := NewRunner(nil, nil); err == nil {
		t.Fatal("want error")
	}
}

func TestRefuseAllAgent(t *testing.T) {
	ans, err := RefuseAllAgent{}.Ask(context.Background(), agent.Question{Text: "x"})
	if err != nil || ans.Outcome != model.OutcomeRefused || ans.RefuseReason != RefuseReasonStub {
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
