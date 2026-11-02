package constraint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

var (
	testNow = time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC) // 本地 10:00
	testLoc = time.FixedZone("CST", 8*3600)
	today   = time.Date(2026, 10, 29, 0, 0, 0, 0, testLoc)

	snapOnce sync.Once
	snapVal  *knowledge.Snapshot
	snapErr  error
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

// repoKB 是仓库内知识库快照（阈值只来自知识库）。
func repoKB(t *testing.T) *knowledge.Snapshot {
	t.Helper()
	snapOnce.Do(func() {
		snapVal, snapErr = knowledge.NewStore(knowledge.NewRepoReader(repoRoot(t))).Open(context.Background(), knowledge.RefWorktree)
	})
	if snapErr != nil {
		t.Fatal(snapErr)
	}
	return snapVal
}

func repoRegulation(t *testing.T) *RegulationMap {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "policy", "regulation_map.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseRegulationMap(b, Default())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// ok 是 OK、带 tool_call_id、1 h 前采集的工具结果。
func ok(tool string, data map[string]any) Source {
	return Source{Tool: tool, Status: "OK", ToolCallID: 7, Source: "fixture", CollectedAt: testNow.Add(-time.Hour), Data: data}
}

func ymd(d time.Time) string { return d.Format(time.DateOnly) }

func intp(v int) *int { return &v }

func f64p(v float64) *float64 { return &v }

// input 是背负式施药、本地单已读取的默认输入；opts 依次修改。
func input(t *testing.T, opts ...func(*Input)) *Input {
	t.Helper()
	in := &Input{Phase: model.PhasePreApproval, WorkType: model.WorkSpray, DeviceType: DeviceKnapsack,
		DispatchMode: model.DispatchDryRun, Now: testNow, Loc: testLoc, KB: repoKB(t), Sources: map[string]Source{},
		LocalLoaded: true}
	for _, o := range opts {
		o(in)
	}
	return in
}

func with(srcs ...Source) func(*Input) {
	return func(in *Input) {
		for _, s := range srcs {
			in.Sources[s.Tool] = s
		}
	}
}

func device(d string) func(*Input) { return func(in *Input) { in.DeviceType = d } }

func work(wt model.WorkType) func(*Input) { return func(in *Input) { in.WorkType = wt } }

func local(orders ...agent.LocalOrder) func(*Input) {
	return func(in *Input) { in.LocalOrders = append(in.LocalOrders, orders...) }
}

func localOrder(id uint64, wt model.WorkType, st model.WorkOrderStatus, at time.Time, product string) agent.LocalOrder {
	payload, err := json.Marshal(map[string]any{"product": product})
	if err != nil {
		panic(err)
	}
	return agent.LocalOrder{OrderID: id, WorkType: wt, Payload: payload, Status: st, UpdatedAt: at}
}

type checkCase struct {
	name string
	in   *Input
	want bool
	// value 非 nil 时断言 observed.value
	value any
}

func runCases(t *testing.T, c Checker, cases []checkCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Check(tc.in)
			if got.Passed != tc.want {
				t.Fatalf("passed = %v, want %v (observed %+v, threshold %+v)", got.Passed, tc.want, got.Observed, got.Threshold)
			}
			if got.Threshold.KB == "" || got.Threshold.ThresholdID == "" {
				t.Errorf("threshold without kb / id: %+v", got.Threshold)
			}
			if tc.value != nil && got.Observed.Value != tc.value {
				t.Errorf("observed.value = %v (%T), want %v (%T)", got.Observed.Value, got.Observed.Value, tc.value, tc.value)
			}
			if got.Passed && c.RuleID() != RuleRegistered && (got.Observed.ToolCallID == 0 || got.Observed.Source == "") {
				t.Errorf("passed without tool_call_id / source: %+v", got.Observed)
			}
		})
	}
}
