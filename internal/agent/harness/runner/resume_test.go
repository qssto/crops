package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

type fakeGate struct {
	err       error
	activated []uint64
}

func (g *fakeGate) Open(context.Context, model.Scope, agent.ApprovalRequest) (model.ApprovalTicket, error) {
	return model.ApprovalTicket{}, errors.New("not used")
}

func (g *fakeGate) Activate(_ context.Context, _ model.Scope, runID uint64) (model.ApprovalTicket, error) {
	g.activated = append(g.activated, runID)
	if g.err != nil {
		return model.ApprovalTicket{}, g.err
	}
	return model.ApprovalTicket{RunID: runID, Status: model.TicketOpen}, nil
}

type fakeResumes struct {
	epoch int
	err   error
	reqs  []agent.ResumeRequest
}

func (f *fakeResumes) MarkResumed(_ context.Context, _ model.Scope, req agent.ResumeRequest) (int, error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return 0, f.err
	}
	f.epoch++
	return f.epoch, nil
}

func newHITLRunner(t *testing.T, runs *fakeRuns, lease *fakeLease, gate agent.ApprovalGate, res agent.RunResumeStore) *Runner {
	t.Helper()
	r, err := New(Config{Locker: fakeLocker{lease: lease}, Runs: runs, Clock: fixedClock{time.Unix(0, 0)},
		RenewEvery: time.Hour, Gate: gate, Resumes: res})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func interruptExec(it model.InterruptType) Exec {
	return func(context.Context, callbacks.Handler) (Outcome, error) {
		return Outcome{Status: model.RunInterrupted, Interrupt: it}, nil
	}
}

// 中断分支：不写终态；WAIT_APPROVAL 激活票据并回填 Ticket，激活失败只告警；缺中断类型报错。
func TestRun_Interrupt(t *testing.T) {
	tests := []struct {
		name       string
		it         model.InterruptType
		gate       *fakeGate
		wantErr    error
		wantTicket bool
		wantActs   int
	}{
		{name: "wait approval activates", it: model.InterruptWaitApproval, gate: &fakeGate{}, wantTicket: true, wantActs: 1},
		{name: "activate failure tolerated", it: model.InterruptWaitApproval, gate: &fakeGate{err: errors.New("down")}, wantActs: 1},
		{name: "no gate", it: model.InterruptWaitApproval},
		{name: "wait input not activated", it: model.InterruptWaitInput, gate: &fakeGate{}},
		{name: "missing interrupt type", it: "", gate: &fakeGate{}, wantErr: ErrBadInterrupt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs, lease := &fakeRuns{}, &fakeLease{}
			var gate agent.ApprovalGate
			if tt.gate != nil {
				gate = tt.gate
			}
			out, err := newHITLRunner(t, runs, lease, gate, nil).Run(context.Background(), model.Scope{}, 9, interruptExec(tt.it))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if len(runs.finished) != 0 {
				t.Errorf("interrupted run finished: %v", runs.finished)
			}
			if (out.Ticket != nil) != tt.wantTicket {
				t.Errorf("ticket = %+v, want present=%v", out.Ticket, tt.wantTicket)
			}
			if tt.gate != nil && len(tt.gate.activated) != tt.wantActs {
				t.Errorf("activations = %v, want %d", tt.gate.activated, tt.wantActs)
			}
			if !lease.released.Load() {
				t.Error("lease not released")
			}
		})
	}
}

// Resume：MarkResumed 的新 epoch 经 Epoch(ctx) 交给执行体；成功写 COMPLETED；守卫失败不执行；执行失败不写 FAILED。
func TestResume(t *testing.T) {
	okExec := func(seen *int) Exec {
		return func(ctx context.Context, _ callbacks.Handler) (Outcome, error) {
			*seen = Epoch(ctx)
			return Outcome{Status: model.RunCompleted}, nil
		}
	}
	t.Run("success", func(t *testing.T) {
		runs, res := &fakeRuns{}, &fakeResumes{epoch: 2}
		var seen int
		out, err := newHITLRunner(t, runs, &fakeLease{}, nil, res).Resume(context.Background(), model.Scope{},
			agent.ResumeRequest{RunID: 9, GraphVersion: "v"}, okExec(&seen))
		if err != nil || out.Status != model.RunCompleted {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		if seen != 3 || len(runs.finished) != 1 || runs.finished[0] != model.RunCompleted {
			t.Errorf("epoch=%d finished=%v, want 3 / [COMPLETED]", seen, runs.finished)
		}
	})
	t.Run("guard refuses", func(t *testing.T) {
		runs, res := &fakeRuns{}, &fakeResumes{err: agent.ErrNotResumable}
		ran := false
		_, err := newHITLRunner(t, runs, &fakeLease{}, nil, res).Resume(context.Background(), model.Scope{},
			agent.ResumeRequest{RunID: 9}, func(context.Context, callbacks.Handler) (Outcome, error) {
				ran = true
				return Outcome{}, nil
			})
		if !errors.Is(err, agent.ErrNotResumable) || ran || len(runs.finished) != 0 {
			t.Errorf("err=%v ran=%v finished=%v", err, ran, runs.finished)
		}
	})
	t.Run("exec failure leaves run for scans", func(t *testing.T) {
		runs, res := &fakeRuns{}, &fakeResumes{}
		boom := errors.New("boom")
		_, err := newHITLRunner(t, runs, &fakeLease{}, nil, res).Resume(context.Background(), model.Scope{},
			agent.ResumeRequest{RunID: 9}, func(context.Context, callbacks.Handler) (Outcome, error) {
				return Outcome{}, boom
			})
		if !errors.Is(err, boom) || len(runs.finished) != 0 {
			t.Errorf("err=%v finished=%v, want boom / none", err, runs.finished)
		}
	})
	t.Run("no store", func(t *testing.T) {
		_, err := newHITLRunner(t, &fakeRuns{}, &fakeLease{}, nil, nil).Resume(context.Background(), model.Scope{},
			agent.ResumeRequest{RunID: 9}, interruptExec(""))
		if !errors.Is(err, ErrNoResumeStore) {
			t.Errorf("err = %v, want ErrNoResumeStore", err)
		}
	})
	if Epoch(context.Background()) != 0 {
		t.Error("Epoch outside runner must be 0")
	}
}
