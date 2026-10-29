package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/model"
)

type fakeFields struct {
	ok  bool
	err error
}

func (f fakeFields) Owns(context.Context, model.Scope, uint64) (bool, error) { return f.ok, f.err }

type fakeLimiter struct {
	allow bool
	err   error
}

func (f fakeLimiter) Allow(context.Context, uint64) (bool, error) { return f.allow, f.err }

type finishCall struct {
	status model.RunStatus
	reason string
}

type fakeRuns struct {
	mu        sync.Mutex
	createErr error
	finishErr error
	created   int
	finished  []finishCall
	summary   model.RunSummary
	sumErr    error
}

func (f *fakeRuns) CreateRun(context.Context, model.RunState, uint64, uint64, agent.RunMeta) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return 0, f.createErr
	}
	f.created++
	return 77, nil
}

func (f *fakeRuns) FinishRun(_ context.Context, _ model.Scope, _ uint64, st model.RunStatus, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, finishCall{st, reason})
	return f.finishErr
}

func (f *fakeRuns) Summary(context.Context, model.Scope, uint64) (model.RunSummary, error) {
	return f.summary, f.sumErr
}

type fakeAdvice struct {
	card model.AdviceCard
	err  error
}

func (f fakeAdvice) GetByRun(context.Context, model.Scope, uint64) (model.AdviceCard, error) {
	return f.card, f.err
}

// fakeAgent 记录收到的 Question；panics 为真时 panic。
type fakeAgent struct {
	mu     sync.Mutex
	got    []agent.Question
	err    error
	panics bool
}

func (f *fakeAgent) Ask(ctx context.Context, q agent.Question) (agent.Answer, error) {
	f.mu.Lock()
	f.got = append(f.got, q)
	f.mu.Unlock()
	if f.panics {
		panic("boom")
	}
	if ctx.Err() != nil {
		return agent.Answer{}, ctx.Err()
	}
	return agent.Answer{Outcome: model.OutcomeAdvice, RunID: q.RunID}, f.err
}

var askScope = model.Scope{TenantID: 1, FarmID: 1, UserID: 1001, Roles: []model.Role{model.RoleFarmManager}}

func TestAskService_Ask(t *testing.T) {
	tests := []struct {
		name       string
		in         AskInput
		fields     fakeFields
		limiter    fakeLimiter
		createErr  error
		agent      *fakeAgent
		wantErr    error
		wantFinish []finishCall
	}{
		{name: "ok async completes without FAILED", in: AskInput{FieldID: 3, Text: "稻飞虱要不要打药"},
			fields: fakeFields{ok: true}, limiter: fakeLimiter{allow: true}, agent: &fakeAgent{}},
		{name: "empty text", in: AskInput{FieldID: 3, Text: "  "}, wantErr: ErrBadInput, agent: &fakeAgent{}},
		{name: "text too long", in: AskInput{FieldID: 3, Text: strings.Repeat("稻", MaxQuestionRunes+1)},
			wantErr: ErrBadInput, agent: &fakeAgent{}},
		{name: "zero field", in: AskInput{Text: "x"}, wantErr: ErrBadInput, agent: &fakeAgent{}},
		{name: "field out of scope", in: AskInput{FieldID: 99, Text: "x"},
			fields: fakeFields{ok: false}, wantErr: ErrForbidden, agent: &fakeAgent{}},
		{name: "field lookup error", in: AskInput{FieldID: 3, Text: "x"},
			fields: fakeFields{err: errors.New("db down")}, agent: &fakeAgent{}},
		{name: "rate limited", in: AskInput{FieldID: 3, Text: "x"},
			fields: fakeFields{ok: true}, limiter: fakeLimiter{allow: false}, wantErr: ErrRateLimited, agent: &fakeAgent{}},
		{name: "create run error", in: AskInput{FieldID: 3, Text: "x"}, fields: fakeFields{ok: true},
			limiter: fakeLimiter{allow: true}, createErr: errors.New("insert"), agent: &fakeAgent{}},
		{name: "async agent error -> FAILED", in: AskInput{FieldID: 3, Text: "x"},
			fields: fakeFields{ok: true}, limiter: fakeLimiter{allow: true}, agent: &fakeAgent{err: errors.New("llm down")},
			wantFinish: []finishCall{{model.RunFailed, reasonAgentError}}},
		{name: "async lease lost -> no FAILED", in: AskInput{FieldID: 3, Text: "x"},
			fields: fakeFields{ok: true}, limiter: fakeLimiter{allow: true},
			agent: &fakeAgent{err: fmt.Errorf("graph: run 77: %w", runner.ErrLeaseLost)}},
		{name: "async agent panic -> FAILED", in: AskInput{FieldID: 3, Text: "x"},
			fields: fakeFields{ok: true}, limiter: fakeLimiter{allow: true}, agent: &fakeAgent{panics: true},
			wantFinish: []finishCall{{model.RunFailed, reasonAgentPanic}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs := &fakeRuns{createErr: tt.createErr}
			svc := NewAskService(AskDeps{Fields: tt.fields, Limiter: tt.limiter, Runs: runs, Advice: fakeAdvice{},
				Agent: tt.agent, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			// 请求 ctx 在返回后立即取消：异步主链路不得受影响。
			ctx, cancel := context.WithCancel(context.Background())
			runID, err := svc.Ask(ctx, askScope, tt.in)
			cancel()
			svc.Wait()
			wantOK := tt.wantErr == nil && tt.createErr == nil && tt.fields.err == nil
			switch {
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			case wantOK && err != nil:
				t.Fatalf("err = %v", err)
			case !wantOK && err == nil:
				t.Fatal("want error")
			}
			if !wantOK {
				if runs.created != 0 || len(tt.agent.got) != 0 {
					t.Fatalf("created=%d asked=%d, want no side effect", runs.created, len(tt.agent.got))
				}
				return
			}
			if runID != 77 || len(tt.agent.got) != 1 {
				t.Fatalf("runID=%d asked=%d", runID, len(tt.agent.got))
			}
			q := tt.agent.got[0]
			if q.RunID != 77 || q.FieldID != tt.in.FieldID || q.Scope.FarmID != askScope.FarmID {
				t.Fatalf("question = %+v", q)
			}
			if len(runs.finished) != len(tt.wantFinish) {
				t.Fatalf("finished = %+v, want %+v", runs.finished, tt.wantFinish)
			}
			for i := range tt.wantFinish {
				if runs.finished[i] != tt.wantFinish[i] {
					t.Fatalf("finished = %+v, want %+v", runs.finished, tt.wantFinish)
				}
			}
		})
	}
}

func TestAskService_GetRun(t *testing.T) {
	sum := model.RunSummary{RunID: 77, Status: model.RunCompleted}
	tests := []struct {
		name     string
		runs     *fakeRuns
		advice   fakeAdvice
		wantErr  error
		wantCard bool
	}{
		{name: "with card", runs: &fakeRuns{summary: sum}, advice: fakeAdvice{card: model.AdviceCard{AdviceID: 5}}, wantCard: true},
		{name: "no card yet", runs: &fakeRuns{summary: sum}, advice: fakeAdvice{err: model.ErrNotFound}},
		{name: "run not in scope", runs: &fakeRuns{sumErr: model.ErrNotFound}, wantErr: model.ErrNotFound},
		{name: "card read error", runs: &fakeRuns{summary: sum}, advice: fakeAdvice{err: errors.New("db")},
			wantErr: errors.New("any")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewAskService(AskDeps{Runs: tt.runs, Advice: tt.advice})
			view, err := svc.GetRun(context.Background(), askScope, 77)
			if tt.wantErr != nil {
				if err == nil || (errors.Is(tt.wantErr, model.ErrNotFound) && !errors.Is(err, model.ErrNotFound)) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (view.Card != nil) != tt.wantCard || view.Run.RunID != 77 {
				t.Fatalf("view = %+v", view)
			}
		})
	}
}
