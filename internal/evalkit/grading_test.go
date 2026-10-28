package evalkit

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// stubGrader 返回固定结果；err 非空时报错。
type stubGrader struct {
	name    string
	pass    bool
	err     error
	gotCard *model.AdviceCard
}

func (g *stubGrader) Name() string  { return g.name }
func (g *stubGrader) RedLine() bool { return true }
func (g *stubGrader) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	g.gotCard = t.Card
	return grader.Score{Pass: g.pass, Value: 1}, g.err
}

// cardAgent 返回带卡片的 REFUSED 回答，并记录收到的提问。
type cardAgent struct{ got *agent.Question }

func (a cardAgent) Ask(_ context.Context, q agent.Question) (agent.Answer, error) {
	*a.got = q
	return agent.Answer{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS", RunID: 9,
		Card: &model.AdviceCard{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}}, nil
}

// failOpener 打开任何 knowledge_ref 都失败。
type failOpener struct{}

func (failOpener) Open(context.Context, string) (*knowledge.Snapshot, error) {
	return nil, errors.New("no such ref")
}

func TestRunner_Grading(t *testing.T) {
	scoped := func(_ context.Context, _ *app.App, s *sample.Sample) (agent.Question, error) {
		q := QuestionOf(s)
		q.Scope, q.FieldID = model.Scope{TenantID: 1, FarmID: 1}, 7
		return q, nil
	}
	tests := []struct {
		name       string
		opts       func(g *stubGrader) []RunnerOption
		grader     *stubGrader
		wantStatus string
		wantErr    string // Trial.Error 或 Score.Error 片段
		wantField  uint64
	}{
		{name: "default outcome_match passes", wantStatus: StatusPass,
			opts: func(*stubGrader) []RunnerOption { return nil }},
		{name: "all graders pass keeps card and scope", grader: &stubGrader{name: "g", pass: true},
			wantStatus: StatusPass, wantField: 7,
			opts: func(g *stubGrader) []RunnerOption {
				return []RunnerOption{WithGraders(nil, g), WithQuestion(scoped)}
			}},
		{name: "failing grader fails", grader: &stubGrader{name: "g"}, wantStatus: StatusFail,
			opts: func(g *stubGrader) []RunnerOption { return []RunnerOption{WithGraders(nil, g)} }},
		{name: "grader error is GRADER_ERROR", grader: &stubGrader{name: "g", pass: true, err: errors.New("bad")},
			wantStatus: StatusGraderError, wantErr: "grader g on a#0: bad",
			opts: func(g *stubGrader) []RunnerOption { return []RunnerOption{WithGraders(nil, g)} }},
		{name: "question error is INFRA_ERROR", wantStatus: StatusInfraError, wantErr: "question: field missing",
			opts: func(*stubGrader) []RunnerOption {
				return []RunnerOption{WithQuestion(func(context.Context, *app.App, *sample.Sample) (agent.Question, error) {
					return agent.Question{}, errors.New("field missing")
				})}
			}},
		{name: "knowledge_ref open error is INFRA_ERROR", grader: &stubGrader{name: "g", pass: true},
			wantStatus: StatusInfraError, wantErr: "no such ref",
			opts: func(g *stubGrader) []RunnerOption { return []RunnerOption{WithGraders(failOpener{}, g)} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var q agent.Question
			r, err := NewRunner(StaticEnv(&app.App{Agent: cardAgent{got: &q}}), nil, tt.opts(tt.grader)...)
			if err != nil {
				t.Fatal(err)
			}
			s := mkSample("a", "regression")
			s.Expect.RefuseReason = "KB_MISS"
			exp, err := r.Run(context.Background(), []*sample.Sample{s}, Options{N: 1})
			if err != nil {
				t.Fatal(err)
			}
			tr := exp.Trials[0]
			if got := tr.Status(); got != tt.wantStatus {
				t.Fatalf("status = %s, want %s; trial = %+v", got, tt.wantStatus, tr)
			}
			errs := tr.Error
			for _, sc := range tr.Scores {
				errs += sc.Error
			}
			if !strings.Contains(errs, tt.wantErr) {
				t.Fatalf("errors = %q, want ~%q", errs, tt.wantErr)
			}
			if q.FieldID != tt.wantField {
				t.Fatalf("question field_id = %d, want %d", q.FieldID, tt.wantField)
			}
			if tt.wantStatus != StatusInfraError && (tr.RunID != 9 || tr.Card == nil) {
				t.Fatalf("run_id / card not kept: %+v", tr)
			}
			if g := tt.grader; g != nil && tt.wantStatus != StatusInfraError && g.gotCard == nil {
				t.Fatal("grader did not receive card")
			}
		})
	}
}

func TestNewRunner_InvalidOptions(t *testing.T) {
	env := StaticEnv(&app.App{Agent: RefuseAllAgent{}})
	for name, opt := range map[string]RunnerOption{"nil question": WithQuestion(nil), "no grader": WithGraders(nil)} {
		if _, err := NewRunner(env, nil, opt); err == nil {
			t.Fatalf("%s: want error", name)
		}
	}
}

func TestTrialStatus(t *testing.T) {
	ok, bad := Score{Name: "a", Pass: true}, Score{Name: "b"}
	tests := []struct {
		name  string
		trial Trial
		want  string
	}{
		{name: "all pass", trial: Trial{Scores: []Score{ok, ok}}, want: StatusPass},
		{name: "one fails", trial: Trial{Scores: []Score{ok, bad}}, want: StatusFail},
		{name: "grader error beats fail", trial: Trial{Scores: []Score{bad, {Name: "c", Error: "x"}}}, want: StatusGraderError},
		{name: "infra error beats all", trial: Trial{Error: "boom", Scores: []Score{ok}}, want: StatusInfraError},
		{name: "unscored is not pass", trial: Trial{}, want: StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trial.Status(); got != tt.want {
				t.Fatalf("Status = %s, want %s", got, tt.want)
			}
		})
	}
}
