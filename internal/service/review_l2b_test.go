package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

const (
	reviewExpert = uint64(9002)
	reviewOther  = uint64(9003)
)

var reviewT0 = time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)

type fakeReviewStore struct {
	tasks       map[uint64]model.ReviewTask
	trial       repository.ReviewTrialSummary
	claimErr    error
	completeErr error
	calls       []string
	lastFilter  model.ReviewTaskFilter
}

func (f *fakeReviewStore) ListReviewTasks(_ context.Context, _ repository.SystemScope, flt model.ReviewTaskFilter) ([]model.ReviewTask, int64, error) {
	f.calls = append(f.calls, "list")
	f.lastFilter = flt
	var out []model.ReviewTask
	for id := uint64(1); id <= uint64(len(f.tasks)); id++ {
		out = append(out, f.tasks[id])
	}
	return out, int64(len(out)), nil
}

func (f *fakeReviewStore) GetReviewTask(_ context.Context, _ repository.SystemScope, id uint64) (model.ReviewTask, error) {
	f.calls = append(f.calls, "get")
	t, ok := f.tasks[id]
	if !ok {
		return model.ReviewTask{}, fmt.Errorf("task %d: %w", id, model.ErrNotFound)
	}
	return t, nil
}

func (f *fakeReviewStore) TrialSummary(_ context.Context, _ repository.SystemScope, t model.ReviewTask) (repository.ReviewTrialSummary, bool, error) {
	f.calls = append(f.calls, "trial")
	return f.trial, t.ExperimentID != nil, nil
}

func (f *fakeReviewStore) ClaimReviewTask(_ context.Context, _ repository.SystemScope, id, expertID uint64) (repository.ReviewClaim, error) {
	f.calls = append(f.calls, "claim")
	if f.claimErr != nil {
		return repository.ReviewClaim{}, f.claimErr
	}
	t := f.tasks[id]
	if t.Status == model.ReviewDone && t.AssigneeID != nil && *t.AssigneeID == expertID {
		return repository.ReviewClaim{Task: t, Done: true}, nil
	}
	t.Status, t.AssigneeID = model.ReviewAssigned, &expertID
	f.tasks[id] = t
	return repository.ReviewClaim{Task: t}, nil
}

func (f *fakeReviewStore) CompleteReviewTask(_ context.Context, _ repository.SystemScope, id, expertID uint64, score *int, at time.Time) error {
	f.calls = append(f.calls, "complete")
	if f.completeErr != nil {
		return f.completeErr
	}
	t := f.tasks[id]
	t.Status, t.AssigneeID, t.ExpertScore, t.DoneAt = model.ReviewDone, &expertID, score, &at
	f.tasks[id] = t
	return nil
}

func (f *fakeReviewStore) SkipReviewTask(_ context.Context, _ repository.SystemScope, id, expertID uint64, at time.Time) error {
	f.calls = append(f.calls, "skip")
	t := f.tasks[id]
	if t.Status != model.ReviewAssigned || t.AssigneeID == nil || *t.AssigneeID != expertID {
		return fmt.Errorf("skip %d: %w", id, repository.ErrReviewConflict)
	}
	t.Status, t.DoneAt = model.ReviewSkipped, &at
	f.tasks[id] = t
	return nil
}

type fakeJudgeWriteback struct {
	row      model.OnlineJudgeResult
	readErr  error
	writeErr error
	gotScope model.Scope
	gotRec   model.AuditRecord
	gotScore int
	gotVer   int
	log      *[]string
}

func (f *fakeJudgeWriteback) JudgeResult(_ context.Context, scope model.Scope, id uint64) (model.OnlineJudgeResult, error) {
	f.gotScope = scope
	if f.readErr != nil {
		return model.OnlineJudgeResult{}, f.readErr
	}
	r := f.row
	r.ID, r.TenantID, r.FarmID = id, scope.TenantID, scope.FarmID
	return r, nil
}

func (f *fakeJudgeWriteback) ReviewJudgeResultAudited(_ context.Context, scope model.Scope, _ uint64, rowVer, score int,
	_ uint64, rec model.AuditRecord, _ time.Time) error {
	*f.log = append(*f.log, "writeback")
	f.gotScope, f.gotRec, f.gotScore, f.gotVer = scope, rec, score, rowVer
	return f.writeErr
}

type fakeReviewAudit struct {
	recs []model.AuditRecord
	err  error
}

func (f *fakeReviewAudit) Append(_ context.Context, r model.AuditRecord) error {
	if f.err != nil {
		return f.err
	}
	f.recs = append(f.recs, r)
	return nil
}

// fakeReviewObjects 模拟 eval 桶：beforePut / afterPut 模拟并发写者在条件写前 / 后写入同一键；
// ignoreCond 模拟对象存储不执行 If-None-Match（照常覆盖）。
type fakeReviewObjects struct {
	objs       map[string][]byte
	beforePut  func(objs map[string][]byte, key string)
	afterPut   func(objs map[string][]byte, key string)
	ignoreCond bool
}

func (f *fakeReviewObjects) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	b, ok := f.objs[bucket+"/"+key]
	if !ok {
		return nil, model.ErrNotFound
	}
	return b, nil
}

func (f *fakeReviewObjects) PutObjectIfAbsent(_ context.Context, bucket, key string, body []byte, _ string) error {
	k := bucket + "/" + key
	if f.beforePut != nil {
		f.beforePut(f.objs, k)
	}
	if _, ok := f.objs[k]; ok && !f.ignoreCond {
		return fmt.Errorf("%s: %w", k, repository.ErrObjectExists)
	}
	f.objs[k] = body
	if f.afterPut != nil {
		f.afterPut(f.objs, k)
	}
	return nil
}

type reviewFixture struct {
	svc   *ReviewService
	store *fakeReviewStore
	judge *fakeJudgeWriteback
	audit *fakeReviewAudit
	objs  *fakeReviewObjects
}

func newReviewFixture(t *testing.T, tasks ...model.ReviewTask) *reviewFixture {
	t.Helper()
	store := &fakeReviewStore{tasks: map[uint64]model.ReviewTask{}, trial: repository.ReviewTrialSummary{Suite: "regression",
		SampleVersion: 2, Status: "PASS"}}
	for i, task := range tasks {
		task.ID = uint64(i + 1)
		store.tasks[task.ID] = task
	}
	f := &reviewFixture{store: store, judge: &fakeJudgeWriteback{log: &store.calls}, audit: &fakeReviewAudit{},
		objs: &fakeReviewObjects{objs: map[string][]byte{}}}
	svc, err := NewReviewService(ReviewDeps{Store: store, Judge: f.judge, Audit: f.audit, Objects: f.objs,
		Bucket: "agri-steward-eval", Mask: func(s string) string { return strings.ReplaceAll(s, "13800138000", "138****8000") },
		Clock: fixedClock{reviewT0}, System: repository.NewSystemScope("review-test")})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func kbExpert(uid uint64) model.Scope {
	return model.Scope{UserID: uid, Roles: []model.Role{model.RoleKBExpert}}
}

func intp(n int) *int { return &n }

func scoreTask() model.ReviewTask {
	exp, idx := uint64(7), 0
	return model.ReviewTask{TaskType: model.ReviewHumanQueue, ExperimentID: &exp, SampleID: "s-open", TrialIndex: &idx,
		Dimension: "fact", RaterNo: 1, Status: model.ReviewOpen}
}

// 红线：kb_expert 以外的角色（含农场上的 kb_expert 绑定）一律拒绝，且不碰评测库。
func TestReviewService_NonExpertDenied(t *testing.T) {
	scopes := []model.Scope{{UserID: 1}, {TenantID: 1, FarmID: 1, UserID: 9002, Roles: []model.Role{model.RoleKBExpert}}}
	for _, r := range []model.Role{model.RoleFarmer, model.RoleFarmManager, model.RoleAgronomist, model.RoleOperator,
		model.RoleAdmin, model.RoleAuditor, model.RolePlatformAdmin, model.RoleLegal} {
		scopes = append(scopes, model.Scope{UserID: 9001, Roles: []model.Role{r}})
	}
	for _, s := range scopes {
		f := newReviewFixture(t, scoreTask())
		_, lerr := f.svc.List(t.Context(), s, ReviewListQuery{})
		_, gerr := f.svc.Get(t.Context(), s, 1)
		_, serr := f.svc.Submit(t.Context(), s, 1, model.ReviewSubmit{Score: intp(2)})
		for _, err := range []error{lerr, gerr, serr} {
			if !errors.Is(err, ErrRoleInsufficient) {
				t.Fatalf("scope %+v: err = %v, want ErrRoleInsufficient", s, err)
			}
		}
		if len(f.store.calls) != 0 {
			t.Fatalf("scope %+v reached store: %v", s, f.store.calls)
		}
	}
	d := DisabledReviewService()
	if _, err := d.Get(t.Context(), model.Scope{UserID: 9001, Roles: []model.Role{model.RolePlatformAdmin}}, 1); !errors.Is(err,
		ErrRoleInsufficient) {
		t.Fatalf("disabled service, non-expert: err = %v", err)
	}
}

// 红线：评测库连接关闭时三个入口都返回 ErrReviewUnavailable（503），不 panic、不回空结果。
func TestReviewService_Disabled(t *testing.T) {
	for _, s := range []*ReviewService{DisabledReviewService(), nil} {
		page, lerr := s.List(t.Context(), kbExpert(reviewExpert), ReviewListQuery{})
		_, gerr := s.Get(t.Context(), kbExpert(reviewExpert), 1)
		_, serr := s.Submit(t.Context(), kbExpert(reviewExpert), 1, model.ReviewSubmit{Score: intp(2)})
		for _, err := range []error{lerr, gerr, serr} {
			if !errors.Is(err, ErrReviewUnavailable) {
				t.Fatalf("err = %v, want ErrReviewUnavailable", err)
			}
		}
		if page.Items != nil || page.Total != 0 {
			t.Fatalf("disabled list returned %+v", page)
		}
	}
	if _, err := NewReviewService(ReviewDeps{}); err == nil {
		t.Fatal("NewReviewService must reject missing deps")
	}
}

// 红线：holdout 任务的列表与详情都不带样本材料，详情也不去读 trial。
func TestReviewService_HoldoutHidden(t *testing.T) {
	hold := scoreTask()
	hold.SampleID, hold.Holdout = "holdout-sample-id", true
	f := newReviewFixture(t, hold, scoreTask())
	got, err := f.svc.Get(t.Context(), kbExpert(reviewExpert), 1)
	if err != nil || got.Material != nil || !got.Holdout {
		t.Fatalf("holdout view = %+v, %v", got, err)
	}
	if slices.Contains(f.store.calls, "trial") {
		t.Fatal("holdout detail read trial")
	}
	page, err := f.svc.List(t.Context(), kbExpert(reviewExpert), ReviewListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "holdout-sample-id") || !strings.Contains(string(b), "s-open") {
		t.Fatalf("list = %s", b)
	}
	open, err := f.svc.Get(t.Context(), kbExpert(reviewExpert), 2)
	if err != nil || open.Material == nil || open.Material.Trial == nil || open.Material.Trial.SampleVersion != 2 {
		t.Fatalf("regular view = %+v, %v", open, err)
	}
}

func TestReviewService_ListQuery(t *testing.T) {
	f := newReviewFixture(t, scoreTask())
	for _, q := range []ReviewListQuery{{TaskType: "OTHER"}, {Status: "LOST"}, {Page: -1}, {PageSize: model.MaxReviewPageSize + 1},
		{PageSize: -1}} {
		if _, err := f.svc.List(t.Context(), kbExpert(reviewExpert), q); !errors.Is(err, ErrBadInput) {
			t.Fatalf("query %+v: err = %v, want ErrBadInput", q, err)
		}
	}
	page, err := f.svc.List(t.Context(), kbExpert(reviewExpert), ReviewListQuery{TaskType: model.ReviewHumanQueue,
		Status: model.ReviewDone, Page: 3, PageSize: 10})
	if err != nil || page.Page != 3 || page.PageSize != 10 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if lf := f.store.lastFilter; lf.Offset != 20 || lf.Limit != 10 || lf.TaskType != model.ReviewHumanQueue ||
		!slices.Equal(lf.Statuses, []string{model.ReviewDone}) {
		t.Fatalf("filter = %+v", lf)
	}
	if _, err := f.svc.List(t.Context(), kbExpert(reviewExpert), ReviewListQuery{}); err != nil ||
		f.store.lastFilter.Limit != model.DefaultReviewPageSize || f.store.lastFilter.Statuses != nil {
		t.Fatalf("default filter = %+v, %v", f.store.lastFilter, err)
	}
}

// 红线：提交幂等（同人同分重放 200 replayed）；不同分重放 / 他人持有 / 乐观锁冲突 → ErrReviewConflict（409）。
func TestReviewService_SubmitScoreIdempotent(t *testing.T) {
	f := newReviewFixture(t, scoreTask())
	ctx, me := t.Context(), kbExpert(reviewExpert)
	res, err := f.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(2)})
	if err != nil || res.Replayed || res.Task.Status != model.ReviewDone || *res.Task.ExpertScore != 2 {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	if len(f.audit.recs) != 1 || f.audit.recs[0].TenantID != 0 || f.audit.recs[0].FarmID != 0 ||
		f.audit.recs[0].Action != string(model.AuditReviewSubmit) || f.audit.recs[0].Target != "review:1" {
		t.Fatalf("audits = %+v", f.audit.recs)
	}
	res, err = f.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(2)})
	// 平台链审计在 Complete 之后写、至少一次：重放也补一条 replayed=true（防 Complete 后审计失败、重试却不再审计）
	if err != nil || !res.Replayed || len(f.audit.recs) != 2 || !strings.Contains(string(f.audit.recs[1].Summary), `"replayed":true`) {
		t.Fatalf("replay = %+v, %v, audits %+v", res, err, f.audit.recs)
	}
	if _, err := f.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(3)}); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("different score replay err = %v", err)
	}
	f.store.claimErr = fmt.Errorf("held: %w", repository.ErrReviewConflict)
	if _, err := f.svc.Submit(ctx, kbExpert(reviewOther), 1, model.ReviewSubmit{Score: intp(2)}); !errors.Is(err,
		ErrReviewConflict) {
		t.Fatalf("claim conflict err = %v", err)
	}
	g := newReviewFixture(t, scoreTask())
	g.store.completeErr = fmt.Errorf("lost: %w", repository.ErrReviewConflict)
	if _, err := g.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(2)}); !errors.Is(err, ErrReviewConflict) ||
		len(g.audit.recs) != 0 {
		t.Fatalf("complete conflict err = %v audits %d", err, len(g.audit.recs))
	}
}

func TestReviewService_SubmitBadInput(t *testing.T) {
	f := newReviewFixture(t, scoreTask())
	for _, s := range []model.ReviewSubmit{{}, {Score: intp(0)}, {Score: intp(4)}, {Score: intp(2), Reason: "x"}} {
		if _, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 1, s); !errors.Is(err, ErrBadInput) ||
			!errors.Is(err, model.ErrReviewSubmit) {
			t.Fatalf("submit %+v err = %v", s, err)
		}
	}
	if _, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 99, model.ReviewSubmit{Score: intp(2)}); !errors.Is(err,
		model.ErrNotFound) {
		t.Fatalf("missing task err = %v", err)
	}
	if slices.Contains(f.store.calls, "claim") {
		t.Fatal("invalid submit claimed the task")
	}
}

// 红线：ONLINE_JUDGE 回写业务库的 scope 取自 draft_ref（tenant / farm），审计记在同一农场链，先业务库后评测库。
func TestReviewService_SubmitOnlineJudge(t *testing.T) {
	task := model.ReviewTask{TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(3, 4, 55), Dimension: "fact",
		JudgeVersion: "j1", RaterNo: 1, Status: model.ReviewOpen}
	judgeRow := func(status string, ver int) model.OnlineJudgeResult {
		return model.OnlineJudgeResult{Dimension: "fact", JudgeVersion: "j1", Status: status, RowVer: ver}
	}
	f := newReviewFixture(t, task)
	f.judge.row = judgeRow(model.JudgeStatusNeedExpert, 6)
	ctx, me := t.Context(), kbExpert(reviewExpert)
	if _, err := f.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(1)}); err != nil {
		t.Fatal(err)
	}
	want := model.Scope{TenantID: 3, FarmID: 4}
	if f.judge.gotScope.TenantID != want.TenantID || f.judge.gotScope.FarmID != want.FarmID || f.judge.gotVer != 6 ||
		f.judge.gotScore != 1 || f.judge.gotRec.TenantID != 3 || f.judge.gotRec.FarmID != 4 ||
		f.judge.gotRec.Action != string(model.AuditReviewSubmit) {
		t.Fatalf("writeback scope %+v ver %d score %d audit %+v", f.judge.gotScope, f.judge.gotVer, f.judge.gotScore, f.judge.gotRec)
	}
	if wi, ci := slices.Index(f.store.calls, "writeback"), slices.Index(f.store.calls, "complete"); wi < 0 || ci < wi {
		t.Fatalf("order = %v, want writeback before complete", f.store.calls)
	}
	if len(f.audit.recs) != 0 {
		t.Fatal("online judge audit must go to the farm chain only")
	}
	// 崩溃后重放：业务库已是本专家同分的 REVIEWED，评测库仍 ASSIGNED → 跳过回写，只补 Complete
	g := newReviewFixture(t, task)
	g.judge.row = judgeRow(model.JudgeStatusReviewed, 7)
	g.judge.row.ExpertScore, g.judge.row.ExpertID = intp(1), reviewExpert
	if res, err := g.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(1)}); err != nil || res.Task.Status != model.ReviewDone {
		t.Fatalf("resume = %+v, %v", res, err)
	}
	if slices.Contains(g.store.calls, "writeback") {
		t.Fatal("resume rewrote business row")
	}
	k := newReviewFixture(t, task)
	k.judge.row = judgeRow(model.JudgeStatusNeedExpert, 6)
	k.judge.writeErr = fmt.Errorf("x: %w", repository.ErrJudgeRowConflict)
	if _, err := k.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(1)}); !errors.Is(err, ErrReviewConflict) ||
		slices.Contains(k.store.calls, "complete") {
		t.Fatalf("row_ver conflict err = %v calls %v", err, k.store.calls)
	}
	bad := task
	bad.DraftRef = "ojr:1:x"
	m := newReviewFixture(t, bad)
	if _, err := m.svc.Submit(ctx, me, 1, model.ReviewSubmit{Score: intp(1)}); err == nil || errors.Is(err, ErrBadInput) {
		t.Fatalf("bad ref err = %v, want internal error", err)
	}
}

// 红线（P1，draft_ref 可伪造）：回写目标须是 NEED_EXPERT 且 dimension / judge_version 与任务一致；否则拒绝、不回写、
// 不 Complete。业务行不存在（含跨 scope：JudgeResult 带 tenant / farm 条件读不到）→ ErrNotFound，同样不回写。
// P2-1：确定性冲突把任务释放为 SKIPPED 并写平台链 REVIEW_SKIP 审计；本人以别的分数写过的行不释放（留给本人重放收尾）。
func TestReviewService_SubmitOnlineJudgeRejectsTarget(t *testing.T) {
	task := model.ReviewTask{TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(3, 4, 55), Dimension: "fact",
		JudgeVersion: "j1", RaterNo: 1, Status: model.ReviewOpen}
	ok := model.OnlineJudgeResult{Dimension: "fact", JudgeVersion: "j1", Status: model.JudgeStatusNeedExpert, RowVer: 2}
	tests := []struct {
		name       string
		row        func(r *model.OnlineJudgeResult)
		readErr    error
		wantErr    error
		wantReason string // 空表示不释放
	}{
		{name: "scored", row: func(r *model.OnlineJudgeResult) { r.Status = model.JudgeStatusScored }, wantErr: ErrReviewConflict,
			wantReason: skipTargetNotPending},
		{name: "reviewed by other", row: func(r *model.OnlineJudgeResult) {
			r.Status, r.ExpertScore, r.ExpertID = model.JudgeStatusReviewed, intp(1), reviewOther
		}, wantErr: ErrReviewConflict, wantReason: skipTargetReviewed},
		{name: "reviewed by me with another score", row: func(r *model.OnlineJudgeResult) {
			r.Status, r.ExpertScore, r.ExpertID = model.JudgeStatusReviewed, intp(3), reviewExpert
		}, wantErr: ErrReviewConflict},
		{name: "unknown status", row: func(r *model.OnlineJudgeResult) { r.Status = "" }, wantErr: ErrReviewConflict,
			wantReason: skipTargetNotPending},
		{name: "dimension mismatch", row: func(r *model.OnlineJudgeResult) { r.Dimension = "citation" }, wantErr: ErrReviewConflict,
			wantReason: skipTargetMismatch},
		{name: "judge version mismatch", row: func(r *model.OnlineJudgeResult) { r.JudgeVersion = "j0" },
			wantErr: ErrReviewConflict, wantReason: skipTargetMismatch},
		{name: "cross scope / missing", readErr: fmt.Errorf("row: %w", model.ErrNotFound), wantErr: model.ErrNotFound,
			wantReason: skipTargetMissing},
		{name: "read failure is not deterministic", readErr: errors.New("db down"), wantErr: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReviewFixture(t, task)
			f.judge.row, f.judge.readErr = ok, tt.readErr
			if tt.row != nil {
				tt.row(&f.judge.row)
			}
			_, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 1, model.ReviewSubmit{Score: intp(1)})
			if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if slices.Contains(f.store.calls, "writeback") || slices.Contains(f.store.calls, "complete") {
				t.Fatalf("rejected target reached writeback / complete: %v", f.store.calls)
			}
			assertReleased(t, f, tt.wantReason)
		})
	}
}

// assertReleased 断言任务释放与否：wantReason 非空时任务为 SKIPPED 且有一条带该原因码的平台链 REVIEW_SKIP 审计；
// 为空时任务仍是本专家的 ASSIGNED、无审计。
func assertReleased(t *testing.T, f *reviewFixture, wantReason string) {
	t.Helper()
	got := f.store.tasks[1]
	if wantReason == "" {
		if got.Status != model.ReviewAssigned || slices.Contains(f.store.calls, "skip") || len(f.audit.recs) != 0 {
			t.Fatalf("must not release: task %s calls %v audits %d", got.Status, f.store.calls, len(f.audit.recs))
		}
		return
	}
	if got.Status != model.ReviewSkipped || len(f.audit.recs) != 1 {
		t.Fatalf("release: task %s audits %+v", got.Status, f.audit.recs)
	}
	rec := f.audit.recs[0]
	if rec.Action != string(model.AuditReviewSkip) || rec.TenantID != 0 || rec.FarmID != 0 || rec.Target != "review:1" ||
		!strings.Contains(string(rec.Summary), `"reason":"`+wantReason+`"`) {
		t.Fatalf("skip audit = %+v", rec)
	}
}

// BACKFLOW_EXPECT：expect / reason 存 eval 桶 review/backflow_expect/{draft_ref}.json，同内容重放 200，不同内容 409；
// 含 PII 拒绝（400）。推进 MERGED / DROPPED 待专家（不在本服务）。
func TestReviewService_SubmitBackflow(t *testing.T) {
	task := model.ReviewTask{TaskType: model.ReviewBackflowExpect, DraftRef: "bf-abcdefghijklmnop", RaterNo: 1, Status: model.ReviewOpen}
	f := newReviewFixture(t, task)
	ctx, me := t.Context(), kbExpert(reviewExpert)
	merge := model.ReviewSubmit{Decision: model.BackflowDecisionMerge, Expect: json.RawMessage(`{"outcome":"ADVICE"}`)}
	if _, err := f.svc.Submit(ctx, me, 1, merge); err != nil {
		t.Fatal(err)
	}
	key := "agri-steward-eval/" + BackflowExpectKey(task.DraftRef)
	if body := f.objs.objs[key]; !strings.Contains(string(body), `"decision":"MERGE"`) || !strings.Contains(string(body), "9002") {
		t.Fatalf("stored %s = %s", key, body)
	}
	if res, err := f.svc.Submit(ctx, me, 1, merge); err != nil || !res.Replayed {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	drop := model.ReviewSubmit{Decision: model.BackflowDecisionDrop, Reason: "重复"}
	if _, err := f.svc.Submit(ctx, me, 1, drop); !errors.Is(err, ErrReviewConflict) {
		t.Fatalf("different decision replay err = %v", err)
	}
	g := newReviewFixture(t, task)
	pii := model.ReviewSubmit{Decision: model.BackflowDecisionDrop, Reason: "联系 13800138000"}
	if _, err := g.svc.Submit(ctx, me, 1, pii); !errors.Is(err, ErrBadInput) || len(g.objs.objs) != 0 {
		t.Fatalf("pii err = %v objs %d", err, len(g.objs.objs))
	}
	piiExpect := model.ReviewSubmit{Decision: model.BackflowDecisionMerge, Expect: json.RawMessage(`{"a":{"b":["13800138000"]}}`)}
	if _, err := g.svc.Submit(ctx, me, 1, piiExpect); !errors.Is(err, ErrBadInput) {
		t.Fatalf("pii expect err = %v", err)
	}
}

// P2-1：BACKFLOW_EXPECT 桶里已有他人结论 → 409 并释放为 SKIPPED（EXPECT_TAKEN）；本人先前的不同结论不释放；
// REVIEW_SKIP 审计写失败时不改任务状态（仍是本专家 ASSIGNED，可重试）。
func TestReviewService_ReleaseBackflow(t *testing.T) {
	task := model.ReviewTask{TaskType: model.ReviewBackflowExpect, DraftRef: "bf-abcdefghijklmnop", RaterNo: 1, Status: model.ReviewOpen}
	drop := model.ReviewSubmit{Decision: model.BackflowDecisionDrop, Reason: "重复"}
	stored := func(t *testing.T, f *reviewFixture, expert uint64) {
		t.Helper()
		b, err := json.Marshal(backflowExpect{DraftRef: task.DraftRef, TaskID: 1, Decision: model.BackflowDecisionMerge,
			Expect: json.RawMessage(`{"outcome":"ADVICE"}`), ExpertID: expert})
		if err != nil {
			t.Fatal(err)
		}
		f.objs.objs["agri-steward-eval/"+BackflowExpectKey(task.DraftRef)] = b
	}
	tests := []struct {
		name       string
		owner      uint64
		auditErr   error
		wantReason string
	}{
		{name: "taken by other", owner: reviewOther, wantReason: skipExpectTaken},
		{name: "mine with another decision", owner: reviewExpert},
		{name: "skip audit fails", owner: reviewOther, auditErr: errors.New("audit down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReviewFixture(t, task)
			stored(t, f, tt.owner)
			f.audit.err = tt.auditErr
			_, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 1, drop)
			if !errors.Is(err, ErrReviewConflict) || (tt.auditErr != nil && !errors.Is(err, tt.auditErr)) {
				t.Fatalf("err = %v", err)
			}
			if slices.Contains(f.store.calls, "complete") {
				t.Fatalf("conflict reached complete: %v", f.store.calls)
			}
			assertReleased(t, f, tt.wantReason)
		})
	}
}

// P2-2：同一 BACKFLOW_EXPECT 并发提交不同结论——条件写只让先写者成功，后写者重读到不同内容 → 409、不 Complete；
// 先写者是本人（同专家两个请求）不释放，是他人释放为 SKIPPED；并发同内容视为重放成功。
func TestReviewService_BackflowConcurrentSubmit(t *testing.T) {
	task := model.ReviewTask{TaskType: model.ReviewBackflowExpect, DraftRef: "bf-abcdefghijklmnop", RaterNo: 1, Status: model.ReviewOpen}
	merge := model.ReviewSubmit{Decision: model.BackflowDecisionMerge, Expect: json.RawMessage(`{"outcome":"ADVICE"}`)}
	winner := func(t *testing.T, decision string, expert uint64) func(map[string][]byte, string) {
		return func(objs map[string][]byte, key string) {
			v := backflowExpect{DraftRef: task.DraftRef, TaskID: 1, Decision: decision, ExpertID: expert}
			if decision == model.BackflowDecisionMerge {
				v.Expect = json.RawMessage(`{"outcome":"ADVICE"}`)
			} else {
				v.Reason = "重复"
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			objs[key] = b
		}
	}
	tests := []struct {
		name       string
		decision   string
		expert     uint64
		wantErr    bool
		wantReason string
	}{
		{name: "mine DROP won", decision: model.BackflowDecisionDrop, expert: reviewExpert, wantErr: true},
		{name: "other DROP won", decision: model.BackflowDecisionDrop, expert: reviewOther, wantErr: true, wantReason: skipExpectTaken},
		{name: "same content won", decision: model.BackflowDecisionMerge, expert: reviewExpert},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReviewFixture(t, task)
			f.objs.beforePut = winner(t, tt.decision, tt.expert)
			_, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 1, merge)
			if !tt.wantErr {
				if err != nil || !slices.Contains(f.store.calls, "complete") {
					t.Fatalf("same-content race = %v, calls %v", err, f.store.calls)
				}
				return
			}
			if !errors.Is(err, ErrReviewConflict) || slices.Contains(f.store.calls, "complete") {
				t.Fatalf("loser err = %v, calls %v", err, f.store.calls)
			}
			key := "agri-steward-eval/" + BackflowExpectKey(task.DraftRef)
			if !strings.Contains(string(f.objs.objs[key]), `"decision":"DROP"`) {
				t.Fatalf("winner overwritten: %s", f.objs.objs[key])
			}
			assertReleased(t, f, tt.wantReason)
		})
	}
	// 兜底：对象存储忽略 If-None-Match，并发写者在本次写入后覆盖——Complete 后重读比对不一致 → 409，不记 REVIEW_SUBMIT
	f := newReviewFixture(t, task)
	f.objs.ignoreCond, f.objs.afterPut = true, winner(t, model.BackflowDecisionDrop, reviewExpert)
	if _, err := f.svc.Submit(t.Context(), kbExpert(reviewExpert), 1, merge); !errors.Is(err, ErrReviewConflict) ||
		len(f.audit.recs) != 0 {
		t.Fatalf("overwritten after put: err = %v audits %d", err, len(f.audit.recs))
	}
}
