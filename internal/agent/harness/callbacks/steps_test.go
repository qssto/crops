package callbacks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent/harness/callbacks"
	"agri-steward/internal/model"
)

var errNode = errors.New("node failed")

func passNode(_ context.Context, s string) (string, error) { return s, nil }

func TestRecorder_Steps(t *testing.T) {
	head := []string{"STEP_STARTED:a:", "STEP_ENDED:a:OK", "STEP_STARTED:b:", "STEP_ENDED:b:OK", "STEP_STARTED:c:"}
	tests := []struct {
		name      string
		last      func(context.Context, string) (string, error)
		withRun   bool
		wantSteps []string // event_type:step_id:status
	}{
		{name: "ok", withRun: true, last: passNode, wantSteps: append(head[:5:5], "STEP_ENDED:c:OK")},
		{name: "node error ends step", withRun: true,
			last:      func(context.Context, string) (string, error) { return "", errNode },
			wantSteps: append(head[:5:5], "STEP_ENDED:c:ERROR")},
		{name: "interrupt ends step", withRun: true,
			last:      func(ctx context.Context, _ string) (string, error) { return "", compose.Interrupt(ctx, "WAIT") },
			wantSteps: append(head[:5:5], "STEP_ENDED:c:INTERRUPTED")},
		{name: "no run in ctx emits nothing", last: passNode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := &sink{}
			rec, err := callbacks.New(callbacks.Config{Events: ev, Raw: &rawStore{}, Clock: &tickClock{}})
			if err != nil {
				t.Fatal(err)
			}
			invoke(t, rec, build(t, rec, tt.last), tt.withRun)
			steps := stepTrail(t, ev.got)
			if len(steps) != len(tt.wantSteps) {
				t.Fatalf("steps = %v, want %v", steps, tt.wantSteps)
			}
			for i := range steps {
				if steps[i] != tt.wantSteps[i] {
					t.Fatalf("steps = %v, want %v", steps, tt.wantSteps)
				}
			}
		})
	}
}

// stepTrail 校验每条事件的 run 标识、payload.node 与 step_id 一致、无孤儿 step，返回 STEP 轨迹。
func stepTrail(t *testing.T, events []model.RunEvent) []string {
	t.Helper()
	var steps []string
	open := map[string]int{}
	for _, e := range events {
		if e.RunID != runID || e.FarmID != scope.FarmID || e.CreatedAt.IsZero() {
			t.Fatalf("event ids = %+v", e)
		}
		if e.Type != model.EventStepStarted && e.Type != model.EventStepEnded {
			continue
		}
		var p model.StepPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Node != e.StepID {
			t.Fatalf("payload node %q != step_id %q", p.Node, e.StepID)
		}
		steps = append(steps, string(e.Type)+":"+e.StepID+":"+p.Status)
		if e.Type == model.EventStepStarted {
			open[e.StepID]++
			continue
		}
		open[e.StepID]--
		if p.LatencyMS <= 0 {
			t.Fatalf("step %s latency = %d", e.StepID, p.LatencyMS)
		}
	}
	for id, n := range open {
		if n != 0 {
			t.Fatalf("orphan step %s (open = %d)", id, n)
		}
	}
	return steps
}
