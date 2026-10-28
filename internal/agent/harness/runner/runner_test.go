package runner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type fakeRuns struct {
	mu         sync.Mutex
	steps      []model.RunStep
	finished   []model.RunStatus
	heartbeats int
	saveErr    error
}

func (f *fakeRuns) CreateRun(context.Context, model.RunState, uint64, uint64, agent.RunMeta) (uint64, error) {
	return 1, nil
}
func (f *fakeRuns) LoadRun(context.Context, model.Scope, uint64) (model.RunState, error) {
	return model.RunState{}, nil
}
func (f *fakeRuns) SaveStep(_ context.Context, _ model.Scope, _ uint64, s model.RunStep) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s)
	return f.saveErr
}
func (f *fakeRuns) FinishRun(_ context.Context, _ model.Scope, _ uint64, st model.RunStatus, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, st)
	return nil
}
func (f *fakeRuns) Heartbeat(context.Context, model.Scope, uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	return nil
}

type fakeLease struct {
	renewErr error
	renews   atomic.Int32
	released atomic.Bool
}

func (l *fakeLease) Renew(context.Context) error   { l.renews.Add(1); return l.renewErr }
func (l *fakeLease) Release(context.Context) error { l.released.Store(true); return nil }

type fakeLocker struct {
	lease *fakeLease
	err   error
}

func (k fakeLocker) AcquireRun(context.Context, uint64) (agent.Lease, error) {
	if k.err != nil {
		return nil, k.err
	}
	return k.lease, nil
}

// twoNodes 是 a → b 的小图；b 的行为由 fn 决定。
func twoNodes(t *testing.T, fn func(ctx context.Context, s string) (string, error)) compose.Runnable[string, string] {
	t.Helper()
	g := compose.NewGraph[string, string]()
	pass := func(_ context.Context, s string) (string, error) { return s, nil }
	if err := g.AddLambdaNode("a", compose.InvokableLambda(pass), compose.WithNodeName("a")); err != nil {
		t.Fatal(err)
	}
	if err := g.AddLambdaNode("b", compose.InvokableLambda(fn), compose.WithNodeName("b")); err != nil {
		t.Fatal(err)
	}
	for _, e := range [][2]string{{compose.START, "a"}, {"a", "b"}, {"b", compose.END}} {
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

func newRunner(t *testing.T, runs *fakeRuns, lease *fakeLease, every time.Duration) *Runner {
	t.Helper()
	r, err := New(Config{Locker: fakeLocker{lease: lease}, Runs: runs, Clock: fixedClock{time.Unix(0, 0)}, RenewEvery: every})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type codedErr struct{}

func (codedErr) Error() string { return "coded" }
func (codedErr) Code() string  { return "DATA_MISSING" }

func TestRun_StepsAndFinish(t *testing.T) {
	tests := []struct {
		name       string
		node       func(context.Context, string) (string, error)
		saveErr    error
		wantStatus model.RunStatus
		wantSteps  []string // node:status
		wantErr    bool
	}{
		{name: "ok", node: func(_ context.Context, s string) (string, error) { return s, nil },
			wantStatus: model.RunCompleted, wantSteps: []string{"a:STARTED", "a:COMPLETED", "b:STARTED", "b:COMPLETED"}},
		{name: "node error", node: func(context.Context, string) (string, error) { return "", codedErr{} },
			wantStatus: model.RunFailed, wantSteps: []string{"a:STARTED", "a:COMPLETED", "b:STARTED", "b:FAILED:DATA_MISSING"}, wantErr: true},
		{name: "save step error fails run", node: func(_ context.Context, s string) (string, error) { return s, nil },
			saveErr: errors.New("db down"), wantStatus: model.RunFailed, wantSteps: []string{"a:STARTED", "a:COMPLETED", "b:STARTED", "b:COMPLETED"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs := &fakeRuns{saveErr: tt.saveErr}
			lease := &fakeLease{}
			graph := twoNodes(t, tt.node)
			out, err := newRunner(t, runs, lease, time.Hour).Run(context.Background(), model.Scope{TenantID: 1}, 7,
				func(ctx context.Context, cb callbacks.Handler) (Outcome, error) {
					if _, err := graph.Invoke(ctx, "x", compose.WithCallbacks(cb)); err != nil {
						return Outcome{}, err
					}
					return Outcome{Status: model.RunCompleted}, nil
				})
			if (err != nil) != tt.wantErr || out.Status != tt.wantStatus {
				t.Fatalf("out = %+v err = %v", out, err)
			}
			var got []string
			for _, s := range runs.steps {
				k := s.Node + ":" + string(s.Status)
				if s.ErrorCode != "" {
					k += ":" + s.ErrorCode
				}
				got = append(got, k)
			}
			if len(got) != len(tt.wantSteps) {
				t.Fatalf("steps = %v, want %v", got, tt.wantSteps)
			}
			for i := range got {
				if got[i] != tt.wantSteps[i] {
					t.Fatalf("steps = %v, want %v", got, tt.wantSteps)
				}
			}
			if len(runs.finished) != 1 || runs.finished[0] != tt.wantStatus || !lease.released.Load() {
				t.Fatalf("finished = %v released = %v", runs.finished, lease.released.Load())
			}
		})
	}
}

func TestRun_RenewFailureStopsWrites(t *testing.T) {
	runs := &fakeRuns{}
	lease := &fakeLease{renewErr: errors.New("token mismatch")}
	var aliveAfter error
	graph := twoNodes(t, func(ctx context.Context, s string) (string, error) {
		<-ctx.Done() // 等续期失败取消 ctx
		aliveAfter = Alive(ctx)
		return s, nil
	})
	_, err := newRunner(t, runs, lease, 5*time.Millisecond).Run(context.Background(), model.Scope{TenantID: 1}, 7,
		func(ctx context.Context, cb callbacks.Handler) (Outcome, error) {
			_, err := graph.Invoke(ctx, "x", compose.WithCallbacks(cb))
			return Outcome{Status: model.RunCompleted}, err
		})
	if !errors.Is(err, ErrLeaseLost) || !errors.Is(aliveAfter, ErrLeaseLost) {
		t.Fatalf("err = %v alive = %v", err, aliveAfter)
	}
	if len(runs.finished) != 0 {
		t.Fatalf("FinishRun called after lease lost: %v", runs.finished)
	}
	for _, s := range runs.steps {
		if s.Node == "b" && s.Status != model.StepStarted {
			t.Fatalf("step written after lease lost: %+v", s)
		}
	}
	if !lease.released.Load() {
		t.Fatal("lease not released")
	}
}

func TestRun_RenewHeartbeats(t *testing.T) {
	runs := &fakeRuns{}
	lease := &fakeLease{}
	_, err := newRunner(t, runs, lease, 2*time.Millisecond).Run(context.Background(), model.Scope{}, 7,
		func(ctx context.Context, _ callbacks.Handler) (Outcome, error) {
			for lease.renews.Load() < 2 {
				select {
				case <-ctx.Done():
					return Outcome{}, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			if err := Alive(ctx); err != nil {
				return Outcome{}, err
			}
			return Outcome{Status: model.RunCompleted}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	runs.mu.Lock()
	defer runs.mu.Unlock()
	if runs.heartbeats < 2 {
		t.Fatalf("heartbeats = %d", runs.heartbeats)
	}
}

func TestRun_AcquireAndGuardErrors(t *testing.T) {
	busy := errors.New("held")
	r, err := New(Config{Locker: fakeLocker{err: busy}, Runs: &fakeRuns{}, Clock: fixedClock{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), model.Scope{}, 1, nil); !errors.Is(err, busy) {
		t.Fatalf("err = %v", err)
	}
	if err := Alive(context.Background()); !errors.Is(err, ErrNoGuard) {
		t.Fatalf("alive = %v", err)
	}
	if _, err := New(Config{RenewEvery: -1}); err == nil {
		t.Fatal("want config error")
	}
}
