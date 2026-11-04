package evalkit

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

func TestConcurrency(t *testing.T) {
	cases := []struct{ qpm, calls, want int }{
		{5, 3, 1},    // 默认（桩模型）：floor(3.5/3) = 1
		{60, 3, 14},  // floor(42/3)
		{100, 7, 10}, // floor(70/7)
		{1, 5, 1},    // 不足 1 取 1
		{0, 3, 1},    // 未配置取 1
		{60, 0, 1},   // 调用数非法取 1
		{1000, 4, 175},
	}
	for _, c := range cases {
		if got := Concurrency(c.qpm, c.calls); got != c.want {
			t.Errorf("Concurrency(%d, %d) = %d, want %d", c.qpm, c.calls, got, c.want)
		}
	}
}

// echoAgent 并发安全：RunID 编码问题文本的长度与 trial 环境序号，用于校验结果没有跨 trial 串。
type echoAgent struct{ env int }

func (a echoAgent) Ask(_ context.Context, q agent.Question) (agent.Answer, error) {
	time.Sleep(time.Millisecond)
	return agent.Answer{Outcome: model.OutcomeAdvice, RefuseReason: q.Text, RunID: uint64(a.env)}, nil
}

// trackingEnv 每个 trial 一个独立 App，记录同时在用的环境数峰值。
type trackingEnv struct {
	inUse, peak atomic.Int64
	next        atomic.Int64
}

func (e *trackingEnv) env(_ context.Context, _ *sample.Sample, _ int) (*app.App, func() error, error) {
	n := e.inUse.Add(1)
	for {
		p := e.peak.Load()
		if n <= p || e.peak.CompareAndSwap(p, n) {
			break
		}
	}
	id := int(e.next.Add(1))
	return &app.App{Agent: echoAgent{env: id}}, func() error { e.inUse.Add(-1); return nil }, nil
}

func samplesN(n int) []*sample.Sample {
	out := make([]*sample.Sample, n)
	for i := range out {
		out[i] = mkSample(fmt.Sprintf("s%02d", i), "regression")
	}
	return out
}

// 并发不改变 trial 顺序与内容：每条 trial 的回答来自它自己的问题，并发峰值不超过 Concurrency（须 -race 跑）。
func TestRunner_ConcurrentKeepsOrderAndIsolation(t *testing.T) {
	samples := samplesN(12)
	for _, c := range []int{1, 4} {
		te := &trackingEnv{}
		r, err := NewRunner(te.env, nil)
		if err != nil {
			t.Fatal(err)
		}
		exp, err := r.Run(context.Background(), samples, Options{N: 3, Concurrency: c})
		if err != nil {
			t.Fatal(err)
		}
		if len(exp.Trials) != 36 {
			t.Fatalf("c=%d trials = %d", c, len(exp.Trials))
		}
		envs := make(map[uint64]bool)
		for i, tr := range exp.Trials {
			want := samples[i/3]
			if tr.SampleID != want.ID || tr.TrialIndex != i%3 || string(tr.RefuseReason) != want.Question {
				t.Fatalf("c=%d trial %d = %s#%d answer %q, want %s#%d", c, i, tr.SampleID, tr.TrialIndex, tr.RefuseReason, want.ID, i%3)
			}
			if envs[tr.RunID] {
				t.Fatalf("c=%d env %d shared by two trials", c, tr.RunID)
			}
			envs[tr.RunID] = true
		}
		if p := te.peak.Load(); p > int64(c) || (c > 1 && p < 2) {
			t.Fatalf("c=%d peak in-flight = %d", c, p)
		}
	}
}

// 默认并发 1 与 L7-a 前的顺序实现逐字节一致（除时间外）：同一输入两种并发的 trial 去掉 RunID / 延迟后相同。
func TestRunner_ConcurrencyDoesNotChangeTrials(t *testing.T) {
	samples := samplesN(6)
	run := func(c int) []Trial {
		te := &trackingEnv{}
		r, err := NewRunner(te.env, nil)
		if err != nil {
			t.Fatal(err)
		}
		exp, err := r.Run(context.Background(), samples, Options{N: 2, Concurrency: c})
		if err != nil {
			t.Fatal(err)
		}
		for i := range exp.Trials {
			exp.Trials[i].RunID, exp.Trials[i].Latency = 0, 0
		}
		return exp.Trials
	}
	if a, b := run(1), run(3); !reflect.DeepEqual(a, b) {
		t.Fatalf("concurrency changed trials:\n%+v\n%+v", a, b)
	}
}

// 预算：累计 token 超过预算后不再派发新 trial，BudgetExceeded = true；不设预算时跑满（应失败用例：无预算逻辑时跑满 6 条）。
func TestRunner_TokenBudget(t *testing.T) {
	samples := samplesN(3)
	count := func(*Trial) int { return 10 }
	cases := []struct {
		budget, conc, wantMax int
		exceeded              bool
	}{
		{0, 1, 6, false},  // 不限
		{60, 1, 6, false}, // 恰好等于不算超
		{25, 1, 3, true},  // 10 + 10 + 10 > 25 后停止
		{25, 2, 4, true},  // 并发时在跑的 trial 跑完，最多多出 conc-1 条
		{55, 1, 6, false}, // 最后一个 trial 才让累计超预算：没有未派发的 trial，不算 BUDGET_EXCEEDED
	}
	for _, c := range cases {
		r, err := NewRunner((&trackingEnv{}).env, nil, WithTokenCounter(count))
		if err != nil {
			t.Fatal(err)
		}
		exp, err := r.Run(context.Background(), samples, Options{N: 2, TokenBudget: c.budget, Concurrency: c.conc})
		if err != nil {
			t.Fatal(err)
		}
		if exp.BudgetExceeded != c.exceeded || len(exp.Trials) > c.wantMax || (!c.exceeded && len(exp.Trials) != 6) ||
			(c.exceeded && len(exp.Trials) < 3) {
			t.Fatalf("budget %d conc %d: trials = %d exceeded = %v", c.budget, c.conc, len(exp.Trials), exp.BudgetExceeded)
		}
	}
	r, err := NewRunner((&trackingEnv{}).env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), samples, Options{N: 1, TokenBudget: -1}); err == nil {
		t.Fatal("negative budget accepted")
	}
}

// RunTrials 只跑指定的 (sample_id, trial_index)，顺序仍按样本与序号。
func TestRunner_RunTrials(t *testing.T) {
	samples := samplesN(3)
	r, err := NewRunner((&trackingEnv{}).env, nil)
	if err != nil {
		t.Fatal(err)
	}
	only := []TrialKey{{"s02", 1}, {"s00", 2}, {"s01", 0}, {"nope", 0}}
	exp, err := r.RunTrials(context.Background(), samples, Options{N: 3}, only)
	if err != nil {
		t.Fatal(err)
	}
	var got []TrialKey
	for _, tr := range exp.Trials {
		got = append(got, TrialKey{tr.SampleID, tr.TrialIndex})
	}
	if want := []TrialKey{{"s00", 2}, {"s01", 0}, {"s02", 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trials = %v, want %v", got, want)
	}
}

// trial panic 交回调用方 goroutine（recordRun 据此置 FAILED），不在 worker 里崩进程。
func TestRunner_ConcurrentPanicPropagates(t *testing.T) {
	var once sync.Once
	env := func(context.Context, *sample.Sample, int) (*app.App, func() error, error) {
		once.Do(func() { panic("boom") })
		return &app.App{Agent: echoAgent{}}, func() error { return nil }, nil
	}
	r, err := NewRunner(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p != "boom" {
			t.Fatalf("recovered %v, want boom", p)
		}
	}()
	if _, err := r.Run(context.Background(), samplesN(4), Options{N: 1, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("no panic")
}

// trial panic 后先停派发再抛出：并发 1 时与顺序执行一致，panic 之后不再起新 trial。
func TestRunner_PanicStopsDispatch(t *testing.T) {
	var calls atomic.Int64
	env := func(context.Context, *sample.Sample, int) (*app.App, func() error, error) {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return &app.App{Agent: echoAgent{}}, func() error { return nil }, nil
	}
	r, err := NewRunner(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p != "boom" {
			t.Fatalf("recovered %v, want boom", p)
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("trials started = %d after panic, want 1", n)
		}
	}()
	if _, err := r.Run(context.Background(), samplesN(4), Options{N: 1, Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("no panic")
}
