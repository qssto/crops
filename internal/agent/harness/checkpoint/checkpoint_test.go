package checkpoint

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// memInner 是 agent.CheckpointStore 的内存实现：拷贝 payload（模拟落库），记录 Set 时 ctx 中的归属。
type memInner struct {
	data    map[string][]byte
	targets []agent.CheckpointTarget
}

func (m *memInner) Get(_ context.Context, id string) ([]byte, bool, error) {
	b, ok := m.data[id]
	return append([]byte(nil), b...), ok, nil
}

func (m *memInner) Set(ctx context.Context, id string, b []byte) error {
	t, _ := agent.CheckpointTargetFrom(ctx)
	m.targets = append(m.targets, t)
	m.data[id] = append([]byte(nil), b...)
	return nil
}

type gateSnap struct{ Snapshot string }

func init() { schema.RegisterName[gateSnap]("checkpoint_adapter_test_snap") }

// 经适配器的 Eino 图：中断时 Set 收到 ctx 中的 CheckpointTarget，payload 拷贝落库后用新编译的图（模拟重启）恢复，
// 中断节点从 interrupt state 取回快照，之前的节点不重跑。
func TestStore_RoundTripThroughEino(t *testing.T) {
	inner := &memInner{data: map[string][]byte{}}
	var firstRuns int
	build := func() compose.Runnable[string, string] {
		g := compose.NewGraph[string, string]()
		must(t, g.AddLambdaNode("first", compose.InvokableLambda(func(_ context.Context, in string) (string, error) {
			firstRuns++
			return in + ">first", nil
		})))
		must(t, g.AddLambdaNode("gate", compose.InvokableLambda(func(ctx context.Context, in string) (string, error) {
			was, has, st := compose.GetInterruptState[gateSnap](ctx)
			if !was {
				return "", compose.StatefulInterrupt(ctx, "WAIT_APPROVAL", gateSnap{Snapshot: in})
			}
			if !has {
				return "", errors.New("no state")
			}
			return st.Snapshot + ">gate", nil
		})))
		must(t, g.AddEdge(compose.START, "first"))
		must(t, g.AddEdge("first", "gate"))
		must(t, g.AddEdge("gate", compose.END))
		r, err := g.Compile(context.Background(), compose.WithCheckPointStore(New(inner)))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	const runID = 42
	target := agent.CheckpointTarget{Scope: model.Scope{TenantID: 1, FarmID: 2}, RunID: runID, Interrupt: model.InterruptWaitApproval}
	ctx := agent.WithCheckpointTarget(context.Background(), target)
	_, err := build().Invoke(ctx, "q", compose.WithCheckPointID(ID(runID)))
	if _, ok := compose.ExtractInterruptInfo(err); !ok {
		t.Fatalf("want interrupt, got %v", err)
	}
	if len(inner.targets) != 1 || !reflect.DeepEqual(inner.targets[0], target) {
		t.Fatalf("Set targets = %+v, want [%+v]", inner.targets, target)
	}
	out, err := build().Invoke(context.Background(), "", compose.WithCheckPointID(ID(runID)))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if out != "q>first>gate" || firstRuns != 1 {
		t.Errorf("out=%q firstRuns=%d, want q>first>gate / 1", out, firstRuns)
	}
}

// Set 拒绝缺少归属、归属 run 与 checkpoint_id 不符或缺中断类型的写入。
func TestStore_SetRequiresTarget(t *testing.T) {
	s := New(&memInner{data: map[string][]byte{}})
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{"no target", context.Background()},
		{"run mismatch", agent.WithCheckpointTarget(context.Background(),
			agent.CheckpointTarget{RunID: 8, Interrupt: model.InterruptWaitApproval})},
		{"no interrupt type", agent.WithCheckpointTarget(context.Background(), agent.CheckpointTarget{RunID: 7})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Set(tt.ctx, ID(7), []byte("x")); !errors.Is(err, ErrNoTarget) {
				t.Errorf("err = %v, want ErrNoTarget", err)
			}
		})
	}
	if New(nil) != nil {
		t.Error("New(nil) must be nil")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
