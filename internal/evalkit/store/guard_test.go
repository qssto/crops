package store

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
)

// 作废规则：infra_error 占比严格大于阈值、或存在 grader_error 即作废；第一条命中的写 void_reason。
func TestVoidReasons(t *testing.T) {
	cases := []struct {
		name string
		c    TrialCounts
		want []string
	}{
		{"clean", TrialCounts{Total: 100}, nil},
		{"no trials", TrialCounts{}, nil},
		{"infra at threshold", TrialCounts{Total: 100, InfraError: 5}, nil},
		{"infra over threshold", TrialCounts{Total: 100, InfraError: 6}, []string{VoidInfraErrorRate}},
		{"one grader error", TrialCounts{Total: 100, GraderError: 1}, []string{VoidGraderError}},
		{"both", TrialCounts{Total: 10, InfraError: 1, GraderError: 1}, []string{VoidInfraErrorRate, VoidGraderError}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VoidReasons(tc.c, 0.05); !slices.Equal(got, tc.want) {
				t.Fatalf("VoidReasons(%+v) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}

// 作废阈值只接受 (0, 5%]：缺失（0）、改松、NaN 一律拒绝（失败关闭）。
func TestCheckVoidRate(t *testing.T) {
	for _, r := range []float64{0.05, 0.01} {
		if err := CheckVoidRate(r); err != nil {
			t.Fatalf("%v: %v", r, err)
		}
	}
	for _, r := range []float64{0, -0.01, 0.0501, 0.5, math.NaN()} {
		if err := CheckVoidRate(r); !errors.Is(err, ErrBadVoidRate) {
			t.Fatalf("%v: err = %v, want ErrBadVoidRate", r, err)
		}
	}
}

func TestSetTokenBudget_Bad(t *testing.T) {
	for _, b := range []int64{0, -1} {
		if err := New(nil).SetTokenBudget(context.Background(), 1, b); !errors.Is(err, ErrBadBudget) {
			t.Fatalf("budget %d: err = %v, want ErrBadBudget", b, err)
		}
	}
}

// resume 只接受已结束且未冻结的实验；RUNNING 与冻结实验拒绝。
func TestRequireResumable(t *testing.T) {
	for _, st := range ResumableStatuses {
		if err := RequireResumable(&Record{ID: 1, Status: st}); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	if err := RequireResumable(&Record{ID: 1, Status: experimentRunning}); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("RUNNING: %v", err)
	}
	if err := RequireResumable(&Record{ID: 1, Status: experimentDone, Frozen: true}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("frozen: %v", err)
	}
}

// VOID / BUDGET_EXCEEDED 不是 DONE：compare / aa / freeze 都经 Done() 拒绝。
func TestGateStatusesNotDone(t *testing.T) {
	now := time.Now()
	for _, st := range []string{StatusVoid, StatusBudgetExceeded} {
		if (&Record{Status: st, FinishedAt: &now}).Done() {
			t.Fatalf("%s counts as DONE", st)
		}
	}
}

// resume 只删本次补跑写回的 (sample_id, trial_index) 上的 INFRA_ERROR 行，未补到的保留。
func TestRerunDelete(t *testing.T) {
	exp := &evalkit.Experiment{Trials: []evalkit.Trial{{SampleID: "a", TrialIndex: 1}, {SampleID: "b", TrialIndex: 0}}}
	q, args := rerunDelete(7, exp)
	want := "DELETE FROM trial WHERE experiment_id = ? AND status = ? AND (sample_id, trial_index) IN ((?, ?), (?, ?))"
	if q != want {
		t.Fatalf("query = %s", q)
	}
	if !slices.Equal(args, []any{uint64(7), StatusInfraError, "a", 1, "b", 0}) {
		t.Fatalf("args = %#v", args)
	}
}
