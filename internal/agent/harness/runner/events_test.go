package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/callbacks"

	"agri-steward/internal/model"
)

type fakeEvents struct {
	mu  sync.Mutex
	got []model.RunEvent
}

func (f *fakeEvents) Emit(_ context.Context, e model.RunEvent) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, e)
	return int64(len(f.got)), nil
}

func TestRun_EndEvents(t *testing.T) {
	tests := []struct {
		name       string
		out        Outcome
		err        error
		wantType   model.EventType
		wantStatus string
		wantReason string
		wantIT     string
		wantErr    bool
	}{
		{name: "completed emits nothing (graph finalize owns RUN_COMPLETED)", out: Outcome{Status: model.RunCompleted}},
		{name: "exec error", err: errors.New("boom"), wantErr: true, wantType: model.EventRunFailed,
			wantStatus: string(model.RunFailed), wantReason: ReasonExecError},
		{name: "exec error keeps reason", out: Outcome{ReasonCode: "LLM_DOWN"}, err: errors.New("boom"), wantErr: true,
			wantType: model.EventRunFailed, wantStatus: string(model.RunFailed), wantReason: "LLM_DOWN"},
		{name: "interrupted", out: Outcome{Status: model.RunInterrupted, Interrupt: model.InterruptWaitApproval},
			wantType: model.EventRunInterrupted, wantStatus: string(model.RunInterrupted), wantIT: string(model.InterruptWaitApproval)},
		{name: "bad interrupt emits nothing", out: Outcome{Status: model.RunInterrupted}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := &fakeEvents{}
			r, err := New(Config{Locker: fakeLocker{lease: &fakeLease{}}, Runs: &fakeRuns{},
				Clock: fixedClock{time.Unix(0, 0)}, RenewEvery: time.Hour, Events: ev})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Run(context.Background(), model.Scope{TenantID: 1, FarmID: 2}, 7,
				func(context.Context, callbacks.Handler) (Outcome, error) { return tt.out, tt.err })
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantType == "" {
				if len(ev.got) != 0 {
					t.Fatalf("events = %+v, want none", ev.got)
				}
				return
			}
			if len(ev.got) != 1 || ev.got[0].Type != tt.wantType || ev.got[0].RunID != 7 || ev.got[0].FarmID != 2 {
				t.Fatalf("events = %+v", ev.got)
			}
			var p model.RunEndPayload
			if err := json.Unmarshal(ev.got[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Status != tt.wantStatus || p.Reason != tt.wantReason || p.InterruptType != tt.wantIT {
				t.Fatalf("payload = %+v", p)
			}
		})
	}
}
