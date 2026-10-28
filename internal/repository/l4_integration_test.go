//go:build integration

package repository_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

var l4Scope = model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID, UserID: 1001}

// jsonEqual 语义比较 JSON（MySQL 读回 json 列会规范化空白与键序）。
func jsonEqual(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("decode %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func TestRunRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	repo := repository.NewRunRepo(slot.DB, slot.Clock)
	ctx := t.Context()
	runID, err := repo.CreateRun(ctx, model.RunState{TenantID: itest.TenantID, FarmID: itest.FarmID}, 1001, 1,
		agent.RunMeta{GraphVersion: "g1", PromptVersion: "p1", ModelTier: "small", KnowledgeRef: "WORKTREE"})
	if err != nil {
		t.Fatal(err)
	}
	end := t0.Add(time.Second)
	for _, st := range []model.RunStep{
		{StepID: "enrich", Node: "enrich", Status: model.StepStarted, StartedAt: t0},
		{StepID: "enrich", Node: "enrich", Status: model.StepCompleted, StartedAt: t0, EndedAt: &end},
		{StepID: "plan", Node: "plan", Status: model.StepStarted, StartedAt: end},
	} {
		if err := repo.SaveStep(ctx, l4Scope, runID, st); err != nil {
			t.Fatal(err)
		}
	}
	st, err := repo.LoadRun(ctx, l4Scope, runID)
	if err != nil || len(st.Steps) != 2 || !reflect.DeepEqual(st.CompletedStepIDs, []string{"enrich"}) {
		t.Fatalf("LoadRun = %+v, %v", st, err)
	}
	if _, err := repo.LoadRun(ctx, model.Scope{TenantID: itest.TenantID, FarmID: 2}, runID); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("LoadRun other farm err = %v, want ErrNotFound", err)
	}
	if err := repo.FinishRun(ctx, l4Scope, runID, model.RunCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishRun(ctx, l4Scope, runID, model.RunFailed, "X"); !errors.Is(err, model.ErrIllegalTransition) {
		t.Fatalf("second FinishRun err = %v, want ErrIllegalTransition", err)
	}
	if s, err := repo.Summary(ctx, l4Scope, runID); err != nil || s.Status != model.RunCompleted {
		t.Fatalf("Summary = %+v, %v", s, err)
	}
}

func TestAdviceRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	repo := repository.NewAdviceRepo(slot.DB, slot.Clock)
	runID := itest.InsertRun(t, slot.DB, t0)
	card := model.AdviceCard{RunID: runID, TenantID: itest.TenantID, FarmID: itest.FarmID, FieldID: 1, Crop: "水稻",
		Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation, Outcome: model.OutcomeAdvice,
		Conclusion: "用吡蚜酮", Reasons: []string{"虫量超阈值"}, NextAction: model.NextCreateOrder,
		Numbers: []model.Number{{Field: "dose", Value: 25, Unit: "mL"}},
		Citations: []model.Citation{
			{Type: model.CitationKB, KBID: "rice/pest/planthopper", KBVersion: "v1"},
			{Type: model.CitationKB, KBID: "rice/pest/planthopper", KBVersion: "v1", ClaimPath: "numbers[0]"},
		}}
	first, err := repo.SaveAdvice(ctx, l4Scope, card)
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.SaveAdvice(ctx, l4Scope, card)
	if err != nil || again != first {
		t.Fatalf("rerun advice_id = %d, %v; want %d", again, err, first)
	}
	counts := []struct {
		name, q string
		want    int64
	}{
		{"one card", "SELECT COUNT(*) FROM advice_card WHERE run_id=?", 1},
		{"citations not duplicated", "SELECT COUNT(*) FROM advice_citation WHERE advice_id=(SELECT id FROM advice_card WHERE run_id=?)", 2},
		{"one memory", "SELECT COUNT(*) FROM episodic_memory WHERE kind='ADVICE' AND ref_id=(SELECT id FROM advice_card WHERE run_id=?)", 1},
	}
	for _, c := range counts {
		if n := itest.Count(t, slot.DB, c.q, runID); n != c.want {
			t.Errorf("%s: count = %d, want %d", c.name, n, c.want)
		}
	}
	// GORM 把 *[]byte 当切片逐元素扫描，单列 JSON 用结构体字段接
	var row struct{ Numbers []byte }
	if err := slot.DB.WithContext(ctx).Raw("SELECT numbers FROM advice_card WHERE id=?", first).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, row.Numbers, `[{"field":"dose","value":25,"unit":"mL"}]`) {
		t.Fatalf("numbers column = %s", row.Numbers)
	}
	got, err := repo.GetByRun(ctx, l4Scope, runID)
	if err != nil || got.AdviceID != first || !reflect.DeepEqual(got.Numbers, card.Numbers) || len(got.Citations) != 2 {
		t.Fatalf("GetByRun = %+v, %v", got, err)
	}

	// 拒答卡不写记忆；scope 不符拒绝写入。
	refusedRun := itest.InsertRun(t, slot.DB, t0)
	refused := model.AdviceCard{RunID: refusedRun, FieldID: 1, Crop: "水稻", Category: model.CategoryProtect,
		Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS", NextAction: model.NextNone}
	refusedID, err := repo.SaveAdvice(ctx, l4Scope, refused)
	if err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, "SELECT COUNT(*) FROM episodic_memory WHERE ref_id=?", refusedID); n != 0 {
		t.Fatalf("refused card memory rows = %d, want 0", n)
	}
	refused.FarmID = 2
	if _, err := repo.SaveAdvice(ctx, l4Scope, refused); !errors.Is(err, repository.ErrScopeMismatch) {
		t.Fatalf("scope mismatch err = %v", err)
	}
}

func TestOrderDraftRepo_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	runID := itest.InsertRun(t, slot.DB, t0)
	adviceID, err := repository.NewAdviceRepo(slot.DB, slot.Clock).SaveAdvice(ctx, l4Scope, model.AdviceCard{
		RunID: runID, FieldID: 1, Crop: "水稻", Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation,
		Outcome: model.OutcomeAdvice, Conclusion: "c", NextAction: model.NextCreateOrder})
	if err != nil {
		t.Fatal(err)
	}
	repo := repository.NewOrderDraftRepo(slot.DB, slot.Clock)
	o := model.WorkOrder{AdviceID: adviceID, RunID: runID, FieldID: 1, WorkType: model.WorkSpray, DeviceType: "DRONE",
		TimeWindow: model.TimeWindow{From: t0, To: t0.Add(time.Hour)}}
	first, err := repo.CreateDraft(ctx, l4Scope, o)
	if err != nil {
		t.Fatal(err)
	}
	again, err := repo.CreateDraft(ctx, l4Scope, o)
	if err != nil || again != first {
		t.Fatalf("rerun order id = %d, %v; want %d", again, err, first)
	}
	for q, want := range map[string]int64{
		"SELECT COUNT(*) FROM work_order WHERE advice_id=? AND status='DRAFT'":                                                       1,
		"SELECT COUNT(*) FROM work_order_transition WHERE order_id=(SELECT id FROM work_order WHERE advice_id=?) AND from_status=''": 1,
	} {
		if n := itest.Count(t, slot.DB, q, adviceID); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	bad := o
	bad.TimeWindow = model.TimeWindow{}
	if _, err := repo.CreateDraft(ctx, l4Scope, bad); !errors.Is(err, repository.ErrBadDraft) {
		t.Fatalf("bad draft err = %v", err)
	}
}

func TestAskLimiterAndRunLocker_Integration(t *testing.T) {
	e := itest.Setup(t, "repo")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	const limit = 2
	lim := repository.NewAskLimiter(e.Redis, slot.Keys, limit)
	for i, want := range []bool{true, true, false} {
		if ok, err := lim.Allow(ctx, itest.FarmID); err != nil || ok != want {
			t.Fatalf("Allow #%d = %v, %v; want %v", i, ok, err, want)
		}
	}
	if ok, err := lim.Allow(ctx, itest.FarmID+1); err != nil || !ok {
		t.Fatalf("other farm Allow = %v, %v", ok, err)
	}

	locker := repository.NewRunLocker(repository.NewLocker(e.Redis), slot.Keys)
	lease, err := locker.AcquireRun(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.AcquireRun(ctx, 1); !errors.Is(err, repository.ErrLockHeld) {
		t.Fatalf("second acquire err = %v, want ErrLockHeld", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	relock, err := locker.AcquireRun(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := relock.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
