package callbacks_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	einocb "github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/callbacks"
	"agri-steward/internal/agent/harness/runner"
	"agri-steward/internal/agent/llm"
	"agri-steward/internal/model"
)

// tickClock 每次 Now 前进 10ms。
type tickClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tickClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(10 * time.Millisecond)
	return c.t
}

type sink struct {
	mu  sync.Mutex
	got []model.RunEvent
}

func (s *sink) Emit(_ context.Context, e model.RunEvent) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
	return int64(len(s.got)), nil
}

type rawStore struct {
	err  error
	puts []model.RawPut
}

func (r *rawStore) Put(_ context.Context, p model.RawPut) (model.RawObject, error) {
	if r.err != nil {
		return model.RawObject{}, r.err
	}
	r.puts = append(r.puts, p)
	return model.RawObject{ID: uint64(100 + len(r.puts))}, nil
}

var scope = model.Scope{TenantID: 1, FarmID: 2}

const runID = 9

// lease / locker / runs 是最小 runner 依赖（让 ctx 带执行权守卫）。
type lease struct{}

func (lease) Renew(context.Context) error   { return nil }
func (lease) Release(context.Context) error { return nil }

type locker struct{}

func (locker) AcquireRun(context.Context, uint64) (agent.Lease, error) { return lease{}, nil }

type runs struct{ agent.RunStateStore }

func (runs) SaveStep(context.Context, model.Scope, uint64, model.RunStep) error { return nil }
func (runs) FinishRun(context.Context, model.Scope, uint64, model.RunStatus, string) error {
	return nil
}
func (runs) Heartbeat(context.Context, model.Scope, uint64) error { return nil }

// build 编译 a → b → c 的图；b 内调用一次模型观测，c 的行为由 last 决定。
func build(t *testing.T, rec *callbacks.Recorder, last func(context.Context, string) (string, error)) compose.Runnable[string, string] {
	t.Helper()
	g := compose.NewGraph[string, string]()
	pass := func(_ context.Context, s string) (string, error) { return s, nil }
	llmNode := func(ctx context.Context, s string) (string, error) {
		rec.ObserveLLM(ctx, llm.Call{Tier: "standard", Task: llm.TaskClassify,
			Messages: []llm.Message{{Role: "user", Content: `{"task":"classify"}`}},
			Attempts: []llm.Attempt{{Reply: `{"in_scope":true}`}},
			Usage:    llm.Usage{PromptTokens: 12, CompletionTokens: 3}, Latency: 40 * time.Millisecond})
		return s, nil
	}
	nodes := []struct {
		name string
		fn   func(context.Context, string) (string, error)
	}{{"a", pass}, {"b", llmNode}, {"c", last}}
	for _, n := range nodes {
		if err := g.AddLambdaNode(n.name, compose.InvokableLambda(n.fn), compose.WithNodeName(n.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range [][2]string{{compose.START, "a"}, {"a", "b"}, {"b", "c"}, {"c", compose.END}} {
		if err := g.AddEdge(e[0], e[1]); err != nil {
			t.Fatal(err)
		}
	}
	r, err := g.Compile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// invoke 在 runner 外壳里跑图（与 graph.Ask 相同：执行体内 WithRun）。
func invoke(t *testing.T, rec *callbacks.Recorder, g compose.Runnable[string, string], withRun bool) {
	t.Helper()
	rn, err := runner.New(runner.Config{Locker: locker{}, Runs: runs{}, Clock: &tickClock{}, RenewEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rn.Run(context.Background(), scope, runID, func(ctx context.Context, _ einocb.Handler) (runner.Outcome, error) {
		if withRun {
			ctx = callbacks.WithRun(ctx, callbacks.Run{Scope: scope, RunID: runID})
		}
		if _, err := g.Invoke(ctx, "x", compose.WithCallbacks(rec.Handler())); err != nil {
			if _, ok := compose.ExtractInterruptInfo(err); ok {
				return runner.Outcome{Status: model.RunInterrupted, Interrupt: model.InterruptWaitApproval}, nil
			}
			return runner.Outcome{}, err
		}
		return runner.Outcome{Status: model.RunCompleted}, nil
	})
	if err != nil && !errors.Is(err, errNode) {
		t.Fatalf("run: %v", err)
	}
}
