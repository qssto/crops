package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

type fakeFacts struct {
	facts    repository.MetricFacts
	err      error
	from, to time.Time
}

func (f *fakeFacts) Facts(_ context.Context, _ model.Scope, from, to time.Time) (repository.MetricFacts, error) {
	f.from, f.to = from, to
	return f.facts, f.err
}

var (
	mDay1 = time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	mDay2 = mDay1.Add(24 * time.Hour)
	mDay3 = mDay2.Add(24 * time.Hour)
)

func at(d time.Time, h int) time.Time { return d.Add(time.Duration(h) * time.Hour) }

func mev(run uint64, seq int, typ model.EventType, payload string) repository.MetricEvent {
	return repository.MetricEvent{RunID: run, Seq: seq, Type: typ, Payload: json.RawMessage(payload)}
}

// metricFixture：day1 两个 run（1 完成、2 恢复后完成并带一次校验失败），day2 一个 run（3 恢复后未完成）。
func metricFixture() repository.MetricFacts {
	opened := at(mDay1, 1)
	return repository.MetricFacts{
		Runs: []repository.MetricRun{
			{ID: 1, Status: model.RunCompleted, CreatedAt: at(mDay1, 1)},
			{ID: 2, Status: model.RunCompleted, CreatedAt: at(mDay1, 2)},
			{ID: 3, Status: model.RunInterrupted, CreatedAt: at(mDay2, 1)},
		},
		Events: []repository.MetricEvent{
			mev(1, 5, model.EventLLMCallEnd, `{"prompt_tokens":100,"completion_tokens":20}`),
			mev(1, 6, model.EventRunCompleted, `{}`),
			mev(2, 3, model.EventValidationFailed, `{}`),
			mev(2, 4, model.EventLLMCallEnd, `{"prompt_tokens":30,"completion_tokens":10}`),
			mev(2, 7, model.EventRunResumed, `{}`),
			mev(2, 9, model.EventRunCompleted, `{}`),
			mev(3, 2, model.EventRunCompleted, `{}`), // 恢复之前的完成不算恢复成功
			mev(3, 4, model.EventRunResumed, `{}`),
			mev(3, 5, model.EventLLMCallEnd, `not-json`),
			mev(99, 1, model.EventRunCompleted, `{}`), // 不在队列内的 run：忽略
		},
		ToolCalls: []repository.MetricToolCall{
			{Status: model.ToolStatusOK, LatencyMS: 100, CreatedAt: at(mDay1, 1)},
			{Status: model.ToolStatusOK, LatencyMS: 300, CreatedAt: at(mDay1, 1)},
			{Status: "TIMEOUT", LatencyMS: 2000, CreatedAt: at(mDay1, 2)},
		},
		Advice: []repository.MetricAdvice{
			{Outcome: model.OutcomeAdvice, CreatedAt: at(mDay1, 1)},
			{Outcome: model.OutcomeRefused, CreatedAt: at(mDay1, 2)},
		},
		Tickets: []repository.MetricTicket{
			{ID: 1, Status: model.TicketApproved, DecidedAt: opened.Add(60 * time.Second), OpenedAt: &opened},
			{ID: 2, Status: model.TicketRejected, DecidedAt: opened.Add(180 * time.Second), OpenedAt: &opened},
			{ID: 3, Status: model.TicketApproved, DecidedAt: at(mDay2, 3)}, // 缺 APPROVAL_PENDING
		},
		Orders: []repository.MetricOrder{
			{ID: 1, Status: model.OrderDone, DispatchMode: string(model.DispatchReal), CreatedAt: at(mDay1, 1), Dispatched: true},
			{ID: 2, Status: model.OrderRejected, DispatchMode: string(model.DispatchReal), CreatedAt: at(mDay1, 2)},
			{ID: 3, Status: model.OrderDone, DispatchMode: string(model.DispatchDryRun), CreatedAt: at(mDay1, 3), Dispatched: true},
		},
	}
}

func val(t *testing.T, name string, r Ratio, num, den int, want *float64) {
	t.Helper()
	if r.Num != num || r.Den != den {
		t.Errorf("%s = %d/%d, want %d/%d", name, r.Num, r.Den, num, den)
	}
	switch {
	case want == nil && r.Value != nil:
		t.Errorf("%s value = %v, want nil", name, *r.Value)
	case want != nil && (r.Value == nil || *r.Value != *want):
		t.Errorf("%s value = %v, want %v", name, r.Value, *want)
	}
}

func f64(v float64) *float64 { return &v }

func TestComputeMetrics(t *testing.T) {
	rep := computeMetrics(metricFixture(), mDay1, mDay3)
	if rep.From != "2026-10-27" || rep.To != "2026-10-29" || len(rep.Days) != 3 {
		t.Fatalf("report range %s..%s days %d", rep.From, rep.To, len(rep.Days))
	}
	d1, d2, d3, tot := rep.Days[0], rep.Days[1], rep.Days[2], rep.Total

	t.Run("sessions and completion", func(t *testing.T) {
		if d1.Sessions != 2 || d2.Sessions != 1 || tot.Sessions != 3 {
			t.Errorf("sessions %d %d %d", d1.Sessions, d2.Sessions, tot.Sessions)
		}
		val(t, "d1 completion", d1.Completion, 2, 2, f64(1))
		val(t, "total completion", tot.Completion, 2, 3, f64(2.0/3))
		val(t, "d1 validation", d1.ValidationFailure, 1, 2, f64(0.5))
	})
	t.Run("resume success needs completed after resume", func(t *testing.T) {
		val(t, "d1 resume", d1.Resume, 1, 1, f64(1))
		val(t, "d2 resume", d2.Resume, 0, 1, f64(0))
		val(t, "total resume", tot.Resume, 1, 2, f64(0.5))
	})
	t.Run("tools", func(t *testing.T) {
		val(t, "tool success", d1.ToolSuccess, 2, 3, f64(2.0/3))
		if d1.ToolP95MS == nil || *d1.ToolP95MS != 2000 {
			t.Errorf("p95 = %v, want 2000", d1.ToolP95MS)
		}
	})
	t.Run("refusal", func(t *testing.T) { val(t, "refusal", d1.Refusal, 1, 2, f64(0.5)) })
	t.Run("approval wait", func(t *testing.T) {
		w := d1.ApprovalWait
		if w.Decided != 2 || w.AvgSec == nil || *w.AvgSec != 120 || w.P95Sec == nil || *w.P95Sec != 180 {
			t.Errorf("d1 wait = %+v", w)
		}
		if w := d2.ApprovalWait; w.Decided != 1 || w.MissingOpen != 1 || w.AvgSec != nil {
			t.Errorf("d2 wait = %+v", w)
		}
	})
	t.Run("tokens", func(t *testing.T) {
		tk := d1.Tokens
		if tk.Prompt != 130 || tk.Completion != 30 || tk.Total != 160 || tk.PerSession == nil || *tk.PerSession != 80 {
			t.Errorf("d1 tokens = %+v", tk)
		}
		if d2.Tokens.BadPayload != 1 || d2.Tokens.Total != 0 {
			t.Errorf("d2 tokens = %+v", d2.Tokens)
		}
	})
	t.Run("dry run excluded from job metrics", func(t *testing.T) {
		j := d1.Jobs
		if j.Orders != 2 || j.DryRunExcluded != 1 {
			t.Errorf("jobs = %+v", j)
		}
		val(t, "dispatch", j.DispatchRate, 1, 2, f64(0.5))
		val(t, "done", j.DoneRate, 1, 2, f64(0.5))
	})
	t.Run("empty day has nil values", func(t *testing.T) {
		if d3.Date != "2026-10-29" || d3.Sessions != 0 || d3.ToolP95MS != nil || d3.Tokens.PerSession != nil ||
			d3.ApprovalWait.AvgSec != nil {
			t.Errorf("d3 = %+v", d3)
		}
		val(t, "d3 completion", d3.Completion, 0, 0, nil)
		val(t, "d3 dispatch", d3.Jobs.DispatchRate, 0, 0, nil)
	})
}

func TestComputeMetrics_Empty(t *testing.T) {
	rep := computeMetrics(repository.MetricFacts{}, mDay1, mDay1)
	if len(rep.Days) != 1 || rep.Total.Date != totalBucket {
		t.Fatalf("rep = %+v", rep)
	}
	b, err := json.Marshal(rep.Total)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["completion_rate"]) != `{"num":0,"den":0,"value":null}` || string(m["tool_p95_latency_ms"]) != "null" {
		t.Errorf("total = %s", b)
	}
}

func TestNearestRank(t *testing.T) {
	tests := []struct {
		in   []float64
		want float64
	}{
		{[]float64{5}, 5},
		{[]float64{3, 1, 2}, 3},
		{[]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, 19},
	}
	for _, tt := range tests {
		if got := nearestRank(tt.in, percentile); got != tt.want {
			t.Errorf("nearestRank(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMetricsService_Daily(t *testing.T) {
	scope := model.Scope{TenantID: 1, FarmID: 1}
	now := at(mDay3, 15)
	tests := []struct {
		name             string
		from, to         time.Time
		err              error
		wantErr          error
		wantFrom, wantTo time.Time
	}{
		{name: "zero range defaults to clock today", wantFrom: mDay3, wantTo: mDay3.Add(24 * time.Hour)},
		{name: "days truncated, to inclusive", from: at(mDay1, 5), to: at(mDay2, 23), wantFrom: mDay1, wantTo: mDay3},
		{name: "to before from", from: mDay2, to: mDay1, wantErr: ErrBadInput},
		{name: "over 31 days", from: mDay1, to: mDay1.Add(31 * 24 * time.Hour), wantErr: ErrBadInput},
		{name: "repo error wrapped", from: mDay1, to: mDay1, err: model.ErrNotFound, wantErr: model.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFacts{err: tt.err}
			_, err := NewMetricsService(f, fixedClock{now}).Daily(context.Background(), scope, tt.from, tt.to)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !f.from.Equal(tt.wantFrom) || !f.to.Equal(tt.wantTo) {
				t.Errorf("facts window [%s, %s), want [%s, %s)", f.from, f.to, tt.wantFrom, tt.wantTo)
			}
		})
	}
}
