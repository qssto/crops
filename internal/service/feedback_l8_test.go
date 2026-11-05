package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
)

// fakeFeedbackStore 模拟 FeedbackRepo：advice → (scope, run)，request_id 全局唯一（uk_request）。
type fakeFeedbackStore struct {
	advices map[uint64]struct {
		farm, run uint64
	}
	rows []model.AdviceFeedback
}

func (f *fakeFeedbackStore) AdviceRun(_ context.Context, scope model.Scope, adviceID uint64) (uint64, error) {
	a, ok := f.advices[adviceID]
	if !ok || a.farm != scope.FarmID {
		return 0, model.ErrNotFound
	}
	return a.run, nil
}

func (f *fakeFeedbackStore) InsertFeedback(_ context.Context, scope model.Scope, fb model.AdviceFeedback) (model.AdviceFeedback, bool, error) {
	for _, r := range f.rows {
		if r.RequestID == fb.RequestID {
			if r.FarmID != scope.FarmID || r.TenantID != scope.TenantID {
				return model.AdviceFeedback{}, false, model.ErrFeedbackRequestConflict
			}
			return r, false, nil
		}
	}
	fb.ID = uint64(len(f.rows) + 1)
	fb.TenantID, fb.FarmID = scope.TenantID, scope.FarmID
	f.rows = append(f.rows, fb)
	return fb, true, nil
}

func newFeedbackFixture(t *testing.T) (*FeedbackService, *fakeFeedbackStore) {
	t.Helper()
	store := &fakeFeedbackStore{advices: map[uint64]struct{ farm, run uint64 }{7: {farm: 1, run: 70}, 8: {farm: 2, run: 80}}}
	svc, err := NewFeedbackService(FeedbackDeps{Store: store, Clock: fixedClock{time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)},
		Mask: func(s string) string { return strings.ReplaceAll(s, "13800138000", "138****8000") }})
	if err != nil {
		t.Fatal(err)
	}
	return svc, store
}

var feedbackScope = model.Scope{TenantID: 1, FarmID: 1, UserID: 1001}

// run_id 取建议卡（不信请求）；comment 落库前经 Mask；user_id 取 Scope。
func TestFeedbackSubmit_RunFromCardAndMasked(t *testing.T) {
	svc, store := newFeedbackFixture(t)
	res, err := svc.Submit(t.Context(), feedbackScope, FeedbackInput{AdviceID: 7, Verdict: model.FeedbackDislike,
		Category: model.RejectOther, Comment: "打 13800138000 问过，药不对", RequestID: "fb-1"})
	if err != nil || res.Replayed {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	got := store.rows[0]
	if got.RunID != 70 || got.UserID != 1001 || got.Category == nil || *got.Category != model.RejectOther {
		t.Fatalf("row = %+v", got)
	}
	if got.Comment == nil || strings.Contains(*got.Comment, "13800138000") {
		t.Fatalf("comment not masked: %v", got.Comment)
	}
	if res, err := svc.Submit(t.Context(), feedbackScope, FeedbackInput{AdviceID: 7, Verdict: model.FeedbackAdopt,
		RequestID: "fb-2"}); err != nil || res.Feedback.Comment != nil || res.Feedback.Category != nil {
		t.Fatalf("adopt = %+v, %v", res, err)
	}
}

// 应失败（红线·幂等）：同 request_id 再提交（哪怕内容不同）返回首次结果、不新增行。
func TestFeedbackSubmit_RequestIDReplay(t *testing.T) {
	svc, store := newFeedbackFixture(t)
	first, err := svc.Submit(t.Context(), feedbackScope, FeedbackInput{AdviceID: 7, Verdict: model.FeedbackAdopt, RequestID: "fb-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.Submit(t.Context(), feedbackScope, FeedbackInput{AdviceID: 7, Verdict: model.FeedbackDislike,
		Category: model.RejectDosage, RequestID: "fb-1"})
	if err != nil || !again.Replayed || again.Feedback.ID != first.Feedback.ID || again.Feedback.Verdict != model.FeedbackAdopt {
		t.Fatalf("replay = %+v, %v; want first result", again, err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(store.rows))
	}
}

// 应失败（红线·隔离）：他场建议卡 → model.ErrNotFound，不写行；入参非法 → ErrBadInput，不查库。
func TestFeedbackSubmit_Rejects(t *testing.T) {
	svc, store := newFeedbackFixture(t)
	if _, err := svc.Submit(t.Context(), feedbackScope, FeedbackInput{AdviceID: 8, Verdict: model.FeedbackAdopt,
		RequestID: "fb-x"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("cross-farm advice = %v, want ErrNotFound", err)
	}
	for _, in := range []FeedbackInput{
		{AdviceID: 7, Verdict: model.FeedbackDislike, RequestID: "fb-y"},
		{AdviceID: 7, Verdict: model.FeedbackDislike, Category: model.RejectOther, RequestID: "fb-y"},
		{AdviceID: 0, Verdict: model.FeedbackAdopt, RequestID: "fb-y"},
		{AdviceID: 7, Verdict: model.FeedbackAdopt},
	} {
		if _, err := svc.Submit(t.Context(), feedbackScope, in); !errors.Is(err, ErrBadInput) || !errors.Is(err, ErrFeedbackInvalid) {
			t.Errorf("submit %+v = %v, want ErrBadInput + FEEDBACK_INVALID", in, err)
		}
	}
	if len(store.rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(store.rows))
	}
}

// P2-L8 权限：advice.feedback 授给持 advice.read 的在场业务角色，只读的 auditor / platform_admin 不授（最小授权）；
// attribution.write 只给 platform_admin；未声明即拒绝。
func TestPermissionMatrix_L8Ops(t *testing.T) {
	m := testPerms(t)
	for _, role := range Roles {
		readOnly := role == model.RoleAuditor || role == model.RolePlatformAdmin
		if got, want := m.Allows(OpAdviceFeedback, role), m.Allows(OpAdviceRead, role) && !readOnly; got != want {
			t.Errorf("advice.feedback for %s = %v, want %v", role, got, want)
		}
		if got := m.Allows(OpAttributionWrite, role); got != (role == model.RolePlatformAdmin) {
			t.Errorf("attribution.write for %s = %v", role, got)
		}
	}
	bare, err := ParsePermissionMatrix([]byte("version: 1\nroles:\n  farmer: [advice.read]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if bare.Allows(OpAdviceFeedback, model.RoleFarmer) {
		t.Error("advice.feedback allowed without declaration")
	}
}
