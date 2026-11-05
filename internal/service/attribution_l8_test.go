package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// fakePatcher 模拟 row_ver 乐观锁：rowVer 不符 → ErrConcurrentUpdate；id 不存在 → ErrNotFound。
type fakePatcher struct {
	rows  map[uint64]*model.AttributionIssue
	calls int
	at    time.Time
}

func (f *fakePatcher) PatchIssue(_ context.Context, _ repository.SystemScope, id uint64, p model.AttributionPatch,
	at time.Time) (model.AttributionIssue, error) {
	f.calls++
	f.at = at
	r, ok := f.rows[id]
	if !ok {
		return model.AttributionIssue{}, model.ErrNotFound
	}
	if *p.RowVer != r.RowVer {
		return model.AttributionIssue{}, model.ErrConcurrentUpdate
	}
	if p.Status != nil {
		r.Status = *p.Status
	}
	r.RowVer++
	return *r, nil
}

// 应失败：row_ver 过期仍改成功（未返回 409 码）、农场用户 / 非 platform_admin 能改、入参非法写库。
func TestAttributionIssueService_Patch(t *testing.T) {
	now := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)
	store := &fakePatcher{rows: map[uint64]*model.AttributionIssue{7: {ID: 7, Status: model.IssueOpen, RowVer: 3}}}
	svc, err := NewAttributionIssueService(AttributionIssueDeps{Store: store, Clock: fixedClock{now},
		System: repository.NewSystemScope("attribution-patch")})
	if err != nil {
		t.Fatal(err)
	}
	ops := model.Scope{UserID: 9001, Roles: []model.Role{model.RolePlatformAdmin}}
	s := func(v string) *string { return &v }
	n := func(v int) *int { return &v }
	ctx := context.Background()

	if _, err := svc.Patch(ctx, ops, 7, model.AttributionPatch{Status: s(model.IssueInProgress), RowVer: n(2)}); !errors.Is(err, ErrIssueConflict) {
		t.Fatalf("stale row_ver err = %v, want ErrIssueConflict", err)
	}
	got, err := svc.Patch(ctx, ops, 7, model.AttributionPatch{Status: s(model.IssueInProgress), RowVer: n(3)})
	if err != nil || got.Status != model.IssueInProgress || got.RowVer != 4 || !store.at.Equal(now) {
		t.Fatalf("patch = %+v, %v (at %s)", got, err, store.at)
	}
	if _, err := svc.Patch(ctx, ops, 8, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(0)}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}

	calls := store.calls
	for name, tc := range map[string]struct {
		actor model.Scope
		id    uint64
		p     model.AttributionPatch
		want  error
	}{
		"farm user":           {model.Scope{TenantID: 1, FarmID: 1, UserID: 1001, Roles: []model.Role{model.RolePlatformAdmin}}, 7, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(4)}, ErrRoleInsufficient},
		"platform non-ops":    {model.Scope{UserID: 9002, Roles: []model.Role{model.RoleAuditor}}, 7, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(4)}, ErrRoleInsufficient},
		"no row_ver":          {ops, 7, model.AttributionPatch{Status: s(model.IssueOpen)}, ErrBadInput},
		"bad status":          {ops, 7, model.AttributionPatch{Status: s("CLOSED"), RowVer: n(4)}, ErrBadInput},
		"zero id":             {ops, 0, model.AttributionPatch{Status: s(model.IssueOpen), RowVer: n(4)}, ErrBadInput},
		"invalid wraps cause": {ops, 7, model.AttributionPatch{RowVer: n(4)}, model.ErrAttributionPatch},
	} {
		if _, err := svc.Patch(ctx, tc.actor, tc.id, tc.p); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if store.calls != calls {
		t.Fatalf("rejected patches reached the store: %d calls", store.calls-calls)
	}
}
