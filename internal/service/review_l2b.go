package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P2-L2b 专家评审（平台路由 /api/v1/admin/review-tasks，review.read / review.submit 只授 kb_expert）。
// 评测库连接（config.eval_review）关闭时用 DisabledReviewService：鉴权照常，之后一律 ErrReviewUnavailable（503）。
//
// 提交的跨库顺序（可重放状态机，见 dev-log P2-L2b）：① 评测库 Claim（OPEN → ASSIGNED 本专家，锁主标 / 双标两行）
// → ② 副作用：ONLINE_JUDGE 先写业务库（online_judge_result 按 row_ver 回写 + 农场链审计，同一事务），BACKFLOW_EXPECT
// 写 eval 桶 expect 对象 → ③ 评测库 Complete（ASSIGNED → DONE）→ ④ 非 ONLINE_JUDGE 写平台链审计。任一步失败
// 任务停在 ASSIGNED（本专家持有），同一专家原样重试会从断点续上：② 发现业务库已是本人同分 / 对象已是同内容即跳过。
// ② 遇到确定性冲突（业务行不存在 / 维度版本不符 / 不是 NEED_EXPERT / 已被他人评过，桶里已有他人结论）时重试不会变，
// 先写平台链 REVIEW_SKIP 审计再把任务释放为 SKIPPED（不回 OPEN：换个专家也会撞上同一冲突）；本人先前写下的不同内容
// 不释放，任务留给本人原样重放收尾。超时回收不做（dev-log 待办）。

// ReviewStore 是评测库 review_task 读写（repository.EvalReviewRepo 实现）。
type ReviewStore interface {
	ListReviewTasks(ctx context.Context, sys repository.SystemScope, f model.ReviewTaskFilter) ([]model.ReviewTask, int64, error)
	GetReviewTask(ctx context.Context, sys repository.SystemScope, id uint64) (model.ReviewTask, error)
	TrialSummary(ctx context.Context, sys repository.SystemScope, t model.ReviewTask) (repository.ReviewTrialSummary, bool, error)
	ClaimReviewTask(ctx context.Context, sys repository.SystemScope, id, expertID uint64) (repository.ReviewClaim, error)
	CompleteReviewTask(ctx context.Context, sys repository.SystemScope, id, expertID uint64, score *int, at time.Time) error
	SkipReviewTask(ctx context.Context, sys repository.SystemScope, id, expertID uint64, at time.Time) error
}

// JudgeWriteback 是业务库 online_judge_result 的读取与专家回写（repository.QualityRepo 实现，条件带 tenant_id / farm_id）。
type JudgeWriteback interface {
	JudgeResult(ctx context.Context, scope model.Scope, id uint64) (model.OnlineJudgeResult, error)
	ReviewJudgeResultAudited(ctx context.Context, scope model.Scope, id uint64, rowVer, score int, expertID uint64,
		rec model.AuditRecord, at time.Time) error
}

// ReviewAuditor 写一条独立审计（repository.AuditRepo 实现）。
type ReviewAuditor interface {
	Append(ctx context.Context, r model.AuditRecord) error
}

// ReviewObjects 是 eval 桶对象读写（repository.QualityObjects 实现；GetObject 不存在返回 model.ErrNotFound，
// PutObjectIfAbsent 已存在返回 repository.ErrObjectExists 且不覆盖）。
type ReviewObjects interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	PutObjectIfAbsent(ctx context.Context, bucket, key string, body []byte, contentType string) error
}

// ReviewDeps 是评审服务依赖；System 由 app 构造（archtest SystemScope 白名单），Mask 注入 guardrail/pii.Mask。
type ReviewDeps struct {
	Store   ReviewStore
	Judge   JudgeWriteback
	Audit   ReviewAuditor
	Objects ReviewObjects
	Bucket  string // agri-steward-eval
	Mask    func(string) string
	Clock   clock.Clock
	System  repository.SystemScope
}

// 评审接口的错误码（controller 原样回给客户端）。
var (
	// ErrReviewUnavailable 是评测库连接未开启（eval_review.enabled=false）→ 503。
	ErrReviewUnavailable = errors.New("EVAL_REVIEW_DISABLED")
	// ErrReviewConflict 是任务 / 业务行状态不允许本次提交（他人持有或已完成、不同内容重放、row_ver 冲突）→ 409。
	ErrReviewConflict = errors.New("REVIEW_CONFLICT")
	// ErrReviewPII 是 BACKFLOW_EXPECT 的 expect / reason 含个人信息（同时是 ErrBadInput）→ 400。
	ErrReviewPII = errors.New("REVIEW_PII")
)

// ReviewListQuery 是列表入参（page 从 1 起；0 值取缺省）。
type ReviewListQuery struct {
	TaskType string
	Status   string
	Page     int
	PageSize int
}

// maxReviewPage 是列表页号上限（防 offset 溢出）。
const maxReviewPage = 1_000_000

// backflowRefPattern 是可作对象键的 draft_ref（backflow 草稿码 bf-xxxx）。
var backflowRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// BackflowExpectKey 是专家对回流草稿结论的 eval 桶对象键。
func BackflowExpectKey(draftRef string) string { return "review/backflow_expect/" + draftRef + ".json" }

const reviewJSON = "application/json"

// ReviewService 是专家评审入口；零值（DisabledReviewService）表示评测库连接关闭。
type ReviewService struct{ d ReviewDeps }

// NewReviewService 校验依赖。
func NewReviewService(d ReviewDeps) (*ReviewService, error) {
	if d.Store == nil || d.Judge == nil || d.Audit == nil || d.Objects == nil || d.Bucket == "" || d.Mask == nil || d.Clock == nil {
		return nil, errors.New("service: review service needs Store, Judge, Audit, Objects, Bucket, Mask and Clock")
	}
	return &ReviewService{d: d}, nil
}

// DisabledReviewService 是评测库连接关闭时挂在路由上的服务：鉴权后一律 ErrReviewUnavailable。
func DisabledReviewService() *ReviewService { return &ReviewService{} }

// enter 是三个入口的公共前置：只认 (0,0) 上的 kb_expert（路由 RequireOps 之外再防御一次），之后才看连接是否开启。
func (s *ReviewService) enter(actor model.Scope) error {
	if actor.TenantID != 0 || actor.FarmID != 0 || actor.UserID == 0 || !actor.HasRole(model.RoleKBExpert) {
		return fmt.Errorf("service: review needs platform kb_expert: %w", ErrRoleInsufficient)
	}
	if s == nil || s.d.Store == nil {
		return fmt.Errorf("service: review: %w", ErrReviewUnavailable)
	}
	return nil
}

// List 分页列任务（缺省只列待评 OPEN / ASSIGNED）；holdout 任务只回 ID 与状态类字段。
func (s *ReviewService) List(ctx context.Context, actor model.Scope, q ReviewListQuery) (model.ReviewTaskPage, error) {
	if err := s.enter(actor); err != nil {
		return model.ReviewTaskPage{}, err
	}
	f, page, size, err := q.filter()
	if err != nil {
		return model.ReviewTaskPage{}, err
	}
	tasks, total, err := s.d.Store.ListReviewTasks(ctx, s.d.System, f)
	if err != nil {
		return model.ReviewTaskPage{}, fmt.Errorf("service: list review tasks: %w", err)
	}
	items := make([]model.ReviewTaskView, 0, len(tasks))
	for _, t := range tasks {
		items = append(items, t.View(actor.UserID))
	}
	return model.ReviewTaskPage{Items: items, Total: total, Page: page, PageSize: size}, nil
}

// filter 校验列表入参并换算成仓储条件。
func (q ReviewListQuery) filter() (model.ReviewTaskFilter, int, int, error) {
	page, size := q.Page, q.PageSize
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = model.DefaultReviewPageSize
	}
	switch {
	case page < 1 || page > maxReviewPage:
		return model.ReviewTaskFilter{}, 0, 0, fmt.Errorf("service: review page %d: %w", q.Page, ErrBadInput)
	case size < 1 || size > model.MaxReviewPageSize:
		return model.ReviewTaskFilter{}, 0, 0, fmt.Errorf("service: review page_size %d (1-%d): %w", q.PageSize,
			model.MaxReviewPageSize, ErrBadInput)
	case q.TaskType != "" && !model.ValidReviewTaskType(q.TaskType):
		return model.ReviewTaskFilter{}, 0, 0, fmt.Errorf("service: review task_type %q: %w", q.TaskType, ErrBadInput)
	case q.Status != "" && !model.ValidReviewStatus(q.Status):
		return model.ReviewTaskFilter{}, 0, 0, fmt.Errorf("service: review status %q: %w", q.Status, ErrBadInput)
	}
	f := model.ReviewTaskFilter{TaskType: q.TaskType, Offset: (page - 1) * size, Limit: size}
	if q.Status != "" {
		f.Statuses = []string{q.Status}
	}
	return f, page, size, nil
}

// Get 读一条任务；非 holdout 再挂 trial 摘要（suite / sample_version / status），holdout 不读 trial。
func (s *ReviewService) Get(ctx context.Context, actor model.Scope, id uint64) (model.ReviewTaskView, error) {
	if err := s.enter(actor); err != nil {
		return model.ReviewTaskView{}, err
	}
	if id == 0 {
		return model.ReviewTaskView{}, fmt.Errorf("service: review task id: %w", ErrBadInput)
	}
	t, err := s.d.Store.GetReviewTask(ctx, s.d.System, id)
	if err != nil {
		return model.ReviewTaskView{}, fmt.Errorf("service: review task %d: %w", id, err)
	}
	v := t.View(actor.UserID)
	if t.Holdout || v.Material == nil {
		return v, nil
	}
	sum, found, err := s.d.Store.TrialSummary(ctx, s.d.System, t)
	if err != nil {
		return model.ReviewTaskView{}, fmt.Errorf("service: review task %d trial: %w", id, err)
	}
	if found {
		v.Material.Trial = &model.ReviewTrial{Suite: sum.Suite, SampleVersion: sum.SampleVersion, Status: sum.Status}
	}
	return v, nil
}

// Submit 提交一条评审（流程见文件头）。入参非法 / 含 PII → ErrBadInput；任务不存在 → model.ErrNotFound；
// 同一专家同内容重放 → Replayed=true（不再产生副作用）；其余状态冲突 → ErrReviewConflict。
func (s *ReviewService) Submit(ctx context.Context, actor model.Scope, id uint64, in model.ReviewSubmit) (model.ReviewSubmitResult, error) {
	if err := s.enter(actor); err != nil {
		return model.ReviewSubmitResult{}, err
	}
	if id == 0 {
		return model.ReviewSubmitResult{}, fmt.Errorf("service: review task id: %w", ErrBadInput)
	}
	t, err := s.d.Store.GetReviewTask(ctx, s.d.System, id)
	if err != nil {
		return model.ReviewSubmitResult{}, fmt.Errorf("service: review task %d: %w", id, err)
	}
	sub, err := s.prepare(t, in)
	if err != nil {
		return model.ReviewSubmitResult{}, err
	}
	claim, err := s.d.Store.ClaimReviewTask(ctx, s.d.System, id, actor.UserID)
	if err != nil {
		return model.ReviewSubmitResult{}, reviewErr(id, "claim", err)
	}
	now := s.d.Clock.Now()
	if claim.Done {
		if err := s.checkReplay(ctx, claim.Task, sub); err != nil {
			return model.ReviewSubmitResult{}, err
		}
	} else {
		if err := s.sideEffect(ctx, actor, claim.Task, sub, now); err != nil {
			var gone targetGone
			if errors.As(err, &gone) {
				if rerr := s.release(ctx, actor, claim.Task, gone, now); rerr != nil {
					return model.ReviewSubmitResult{}, errors.Join(err, rerr)
				}
			}
			return model.ReviewSubmitResult{}, err
		}
		if err := s.d.Store.CompleteReviewTask(ctx, s.d.System, id, actor.UserID, sub.score, now); err != nil {
			return model.ReviewSubmitResult{}, reviewErr(id, "complete", err)
		}
		if t.TaskType == model.ReviewBackflowExpect { // 兜底：对象存储若未执行条件写，Complete 后重读比对
			if err := s.verifyExpect(ctx, claim.Task, sub, actor.UserID); err != nil {
				return model.ReviewSubmitResult{}, err
			}
		}
	}
	if t.TaskType != model.ReviewOnlineJudge { // ④ 平台链审计（至少一次：重放也补一条）
		payload, err := sub.auditPayload(ctx, t.TaskType, claim.Done)
		if err != nil {
			return model.ReviewSubmitResult{}, fmt.Errorf("service: review task %d audit: %w", id, err)
		}
		rec := repository.NewAuditRecord(model.Scope{}, nil, model.Actor{Type: model.ActorUser, ID: actor.UserID},
			model.AuditReviewSubmit, reviewTarget(id), payload, model.AuditResultOK, now)
		if err := s.d.Audit.Append(ctx, rec); err != nil {
			return model.ReviewSubmitResult{}, fmt.Errorf("service: review task %d audit: %w", id, err)
		}
	}
	done := claim.Task
	done.Status, done.ExpertScore = model.ReviewDone, sub.score
	if !claim.Done {
		done.DoneAt = &now
	}
	return model.ReviewSubmitResult{Task: done.View(actor.UserID), Replayed: claim.Done}, nil
}

// reviewSubmission 是校验后的提交：score 写 review_task.expert_score（BACKFLOW_EXPECT 为 nil）；
// judge 为 ONLINE_JUDGE 的回写目标；expect 为 BACKFLOW_EXPECT 规范化（键排序、紧凑）后的 expect。
type reviewSubmission struct {
	in     model.ReviewSubmit
	score  *int
	judge  model.OnlineJudgeTarget
	expect json.RawMessage
}

// prepare 在 Claim 之前做完一切不碰库的校验：提交体、ONLINE_JUDGE 引用、BACKFLOW_EXPECT 的 PII。
func (s *ReviewService) prepare(t model.ReviewTask, in model.ReviewSubmit) (reviewSubmission, error) {
	if err := model.ValidateReviewSubmit(t.TaskType, in); err != nil {
		return reviewSubmission{}, fmt.Errorf("service: review task %d: %w: %w", t.ID, ErrBadInput, err)
	}
	sub := reviewSubmission{in: in, score: in.Score}
	switch t.TaskType {
	case model.ReviewOnlineJudge:
		tn, fm, rid, err := model.ParseOnlineJudgeRef(t.DraftRef)
		if err != nil { // 入队数据损坏：不是专家的错，回 500
			return reviewSubmission{}, fmt.Errorf("service: review task %d: %w", t.ID, err)
		}
		sub.judge = model.OnlineJudgeTarget{TenantID: tn, FarmID: fm, ResultID: rid}
	case model.ReviewBackflowExpect:
		if !backflowRefPattern.MatchString(t.DraftRef) {
			return reviewSubmission{}, fmt.Errorf("service: review task %d: draft_ref %q not an object key", t.ID, t.DraftRef)
		}
		if len(in.Expect) > 0 {
			v, err := decodeJSON(in.Expect)
			if err != nil {
				return reviewSubmission{}, fmt.Errorf("service: review task %d expect: %w: %w", t.ID, ErrBadInput, err)
			}
			if sub.expect, err = json.Marshal(v); err != nil {
				return reviewSubmission{}, fmt.Errorf("service: review task %d expect: %w", t.ID, err)
			}
			if s.hasPII(v) {
				return reviewSubmission{}, fmt.Errorf("service: review task %d expect: %w: %w", t.ID, ErrBadInput, ErrReviewPII)
			}
		}
		if s.d.Mask(in.Reason) != in.Reason {
			return reviewSubmission{}, fmt.Errorf("service: review task %d reason: %w: %w", t.ID, ErrBadInput, ErrReviewPII)
		}
		sub.score = nil
	}
	return sub, nil
}

// sideEffect 是第 ② 步：ONLINE_JUDGE 回写业务库，BACKFLOW_EXPECT 写 eval 桶，其余无。
func (s *ReviewService) sideEffect(ctx context.Context, actor model.Scope, t model.ReviewTask, sub reviewSubmission, now time.Time) error {
	switch t.TaskType {
	case model.ReviewOnlineJudge:
		return s.writeJudge(ctx, actor, t, sub, now)
	case model.ReviewBackflowExpect:
		return s.putExpect(ctx, actor, t, sub, now)
	}
	return nil
}

// writeJudge 按 draft_ref 的 tenant / farm 回写 online_judge_result（row_ver 乐观锁，同事务写农场链审计）。
// draft_ref 只是定位（评测库写权限即可伪造），所以先核对业务行：dimension / judge_version 须与任务一致，且只回写
// NEED_EXPERT 行（UPDATE 也带 status 条件）。业务行已是本专家同分的 REVIEWED（上次在 Complete 前中断）跳过；
// 被别人或别的分数评过、SCORED 等其余状态 → ErrReviewConflict，业务行不动。
func (s *ReviewService) writeJudge(ctx context.Context, actor model.Scope, t model.ReviewTask, sub reviewSubmission, now time.Time) error {
	scope := model.Scope{TenantID: sub.judge.TenantID, FarmID: sub.judge.FarmID}
	row, err := s.d.Judge.JudgeResult(ctx, scope, sub.judge.ResultID)
	if errors.Is(err, model.ErrNotFound) {
		return targetGone{reason: skipTargetMissing, err: fmt.Errorf("service: review task %d judge row: %w", t.ID, err)}
	}
	if err != nil {
		return fmt.Errorf("service: review task %d judge row: %w", t.ID, err)
	}
	if row.Dimension != t.Dimension || row.JudgeVersion != t.JudgeVersion {
		return targetGone{reason: skipTargetMismatch, err: fmt.Errorf("service: review task %d judge row %d is %s/%s, task %s/%s: %w",
			t.ID, row.ID, row.Dimension, row.JudgeVersion, t.Dimension, t.JudgeVersion, ErrReviewConflict)}
	}
	switch row.Status {
	case model.JudgeStatusNeedExpert:
	case model.JudgeStatusReviewed:
		if row.ExpertID == actor.UserID && equalScore(row.ExpertScore, sub.score) {
			return nil
		}
		err := fmt.Errorf("service: review task %d judge row %d already reviewed: %w", t.ID, row.ID, ErrReviewConflict)
		if row.ExpertID == actor.UserID { // 本人先前以别的分数写过：不释放，留给本人原样重放收尾
			return err
		}
		return targetGone{reason: skipTargetReviewed, err: err}
	default:
		return targetGone{reason: skipTargetNotPending, err: fmt.Errorf("service: review task %d judge row %d is %s, not %s: %w",
			t.ID, row.ID, row.Status, model.JudgeStatusNeedExpert, ErrReviewConflict)}
	}
	payload, err := sub.auditPayload(ctx, t.TaskType, false)
	if err != nil {
		return fmt.Errorf("service: review task %d audit: %w", t.ID, err)
	}
	rec := repository.NewAuditRecord(scope, nil, model.Actor{Type: model.ActorUser, ID: actor.UserID}, model.AuditReviewSubmit,
		reviewTarget(t.ID), payload, model.AuditResultOK, now)
	if err := s.d.Judge.ReviewJudgeResultAudited(ctx, scope, row.ID, row.RowVer, *sub.score, actor.UserID, rec, now); err != nil {
		return reviewErr(t.ID, "judge writeback", err)
	}
	return nil
}

// backflowExpect 是 eval 桶 review/backflow_expect/{draft_ref}.json 的内容（后续 MERGED / DROPPED 推进读它，待专家）。
type backflowExpect struct {
	DraftRef    string          `json:"draft_ref"`
	TaskID      uint64          `json:"task_id"`
	Decision    string          `json:"decision"`
	Expect      json.RawMessage `json:"expect,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	ExpertID    uint64          `json:"expert_id"`
	SubmittedAt time.Time       `json:"submitted_at"`
}

// same 比较专家结论（不比提交时刻）。
func (b backflowExpect) same(o backflowExpect) bool {
	return b.DraftRef == o.DraftRef && b.TaskID == o.TaskID && b.Decision == o.Decision && bytes.Equal(b.Expect, o.Expect) &&
		b.Reason == o.Reason && b.ExpertID == o.ExpertID
}

func (sub reviewSubmission) backflow(t model.ReviewTask, expertID uint64, now time.Time) backflowExpect {
	return backflowExpect{DraftRef: t.DraftRef, TaskID: t.ID, Decision: sub.in.Decision, Expect: sub.expect,
		Reason: sub.in.Reason, ExpertID: expertID, SubmittedAt: now.UTC()}
}

// storedExpect 读已存的结论；不存在 found=false。
func (s *ReviewService) storedExpect(ctx context.Context, draftRef string) (backflowExpect, bool, error) {
	b, err := s.d.Objects.GetObject(ctx, s.d.Bucket, BackflowExpectKey(draftRef))
	if errors.Is(err, model.ErrNotFound) {
		return backflowExpect{}, false, nil
	}
	if err != nil {
		return backflowExpect{}, false, fmt.Errorf("service: read backflow expect %s: %w", draftRef, err)
	}
	var got backflowExpect
	if err := json.Unmarshal(b, &got); err != nil {
		return backflowExpect{}, false, fmt.Errorf("service: decode backflow expect %s: %w", draftRef, err)
	}
	return got, true, nil
}

// putExpect 条件写专家结论（对象不存在才写，并发提交只有一个写入成功）：已有或并发写入了同内容（上次在 Complete 前
// 中断 / 同内容并发重放）跳过，已有不同内容 → ErrReviewConflict（他人的结论释放任务，本人的不释放）。
func (s *ReviewService) putExpect(ctx context.Context, actor model.Scope, t model.ReviewTask, sub reviewSubmission, now time.Time) error {
	want := sub.backflow(t, actor.UserID, now)
	got, found, err := s.storedExpect(ctx, t.DraftRef)
	if err != nil {
		return err
	}
	if !found {
		body, err := json.Marshal(want)
		if err != nil {
			return fmt.Errorf("service: encode backflow expect %s: %w", t.DraftRef, err)
		}
		err = s.d.Objects.PutObjectIfAbsent(ctx, s.d.Bucket, BackflowExpectKey(t.DraftRef), body, reviewJSON)
		if err == nil {
			return nil
		}
		if !errors.Is(err, repository.ErrObjectExists) {
			return fmt.Errorf("service: put backflow expect %s: %w", t.DraftRef, err)
		}
		if got, found, err = s.storedExpect(ctx, t.DraftRef); err != nil { // 并发写者先写入：重读比对
			return err
		}
		if !found {
			return fmt.Errorf("service: review task %d: backflow expect vanished after conflict: %w", t.ID, ErrReviewConflict)
		}
	}
	if got.same(want) {
		return nil
	}
	err = fmt.Errorf("service: review task %d: backflow expect differs: %w", t.ID, ErrReviewConflict)
	if got.ExpertID == actor.UserID { // 本人先前写下的不同结论：不释放，留给本人原样重放收尾
		return err
	}
	return targetGone{reason: skipExpectTaken, err: err}
}

// verifyExpect 重读桶里的结论，须与本次提交一致（重放判定与 Complete 后兜底比对共用）；不一致 → ErrReviewConflict。
func (s *ReviewService) verifyExpect(ctx context.Context, t model.ReviewTask, sub reviewSubmission, expertID uint64) error {
	got, found, err := s.storedExpect(ctx, t.DraftRef)
	if err != nil {
		return err
	}
	if !found || !got.same(sub.backflow(t, expertID, time.Time{})) {
		return fmt.Errorf("service: review task %d done with another conclusion: %w", t.ID, ErrReviewConflict)
	}
	return nil
}

// checkReplay 判定本专家已完成任务上的再次提交是否为原样重放；不同内容 → ErrReviewConflict。
func (s *ReviewService) checkReplay(ctx context.Context, t model.ReviewTask, sub reviewSubmission) error {
	if t.TaskType != model.ReviewBackflowExpect {
		if equalScore(t.ExpertScore, sub.score) {
			return nil
		}
		return fmt.Errorf("service: review task %d done with another score: %w", t.ID, ErrReviewConflict)
	}
	if t.AssigneeID == nil {
		return fmt.Errorf("service: review task %d done without assignee: %w", t.ID, ErrReviewConflict)
	}
	return s.verifyExpect(ctx, t, sub, *t.AssigneeID)
}

// reviewAudit 是 REVIEW_SUBMIT 审计摘要：不含样本 ID、expect / reason 原文。
type reviewAudit struct {
	TaskType  string `json:"task_type"`
	RequestID string `json:"request_id,omitempty"`
	Score     *int   `json:"score,omitempty"`
	Decision  string `json:"decision,omitempty"`
	ResultID  uint64 `json:"online_judge_result_id,omitempty"`
	Replayed  bool   `json:"replayed"`
}

func (sub reviewSubmission) auditPayload(ctx context.Context, taskType string, replayed bool) (json.RawMessage, error) {
	b, err := json.Marshal(reviewAudit{TaskType: taskType, RequestID: RequestIDFrom(ctx), Score: sub.score,
		Decision: sub.in.Decision, ResultID: sub.judge.ResultID, Replayed: replayed})
	if err != nil {
		return nil, fmt.Errorf("service: encode review audit: %w", err)
	}
	return b, nil
}

func reviewTarget(id uint64) string { return fmt.Sprintf("review:%d", id) }

// 任务释放为 SKIPPED 的原因码（REVIEW_SKIP 审计 summary.reason）。
const (
	skipTargetMissing    = "TARGET_MISSING"     // ONLINE_JUDGE：业务行不存在（含 draft_ref 指向别的农场）
	skipTargetMismatch   = "TARGET_MISMATCH"    // ONLINE_JUDGE：业务行 dimension / judge_version 与任务不符
	skipTargetReviewed   = "TARGET_REVIEWED"    // ONLINE_JUDGE：业务行已被他人评过
	skipTargetNotPending = "TARGET_NOT_PENDING" // ONLINE_JUDGE：业务行不是 NEED_EXPERT（如 SCORED）
	skipExpectTaken      = "EXPECT_TAKEN"       // BACKFLOW_EXPECT：桶里已有他人的结论
)

// targetGone 是副作用遇到的确定性冲突：同一专家重试也不会变，Submit 据此释放任务；err 原样向上（409 / 404）。
type targetGone struct {
	reason string
	err    error
}

func (g targetGone) Error() string { return g.err.Error() }
func (g targetGone) Unwrap() error { return g.err }

// reviewSkipAudit 是 REVIEW_SKIP 审计摘要：不含样本 ID、draft_ref。
type reviewSkipAudit struct {
	TaskType  string `json:"task_type"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason"`
}

// release 把本专家持有的任务释放为 SKIPPED：先写平台链 REVIEW_SKIP 审计（审计失败任务仍是 ASSIGNED，不丢痕），
// 再按 status=ASSIGNED AND assignee_id=本专家 条件改状态。
func (s *ReviewService) release(ctx context.Context, actor model.Scope, t model.ReviewTask, gone targetGone, now time.Time) error {
	payload, err := json.Marshal(reviewSkipAudit{TaskType: t.TaskType, RequestID: RequestIDFrom(ctx), Reason: gone.reason})
	if err != nil {
		return fmt.Errorf("service: encode review skip audit: %w", err)
	}
	rec := repository.NewAuditRecord(model.Scope{}, nil, model.Actor{Type: model.ActorUser, ID: actor.UserID},
		model.AuditReviewSkip, reviewTarget(t.ID), payload, model.AuditResultOK, now)
	if err := s.d.Audit.Append(ctx, rec); err != nil {
		return fmt.Errorf("service: review task %d skip audit: %w", t.ID, err)
	}
	if err := s.d.Store.SkipReviewTask(ctx, s.d.System, t.ID, actor.UserID, now); err != nil {
		return fmt.Errorf("service: review task %d skip: %w", t.ID, err)
	}
	return nil
}

// reviewErr 把仓储的状态冲突（review_task 状态 / online_judge_result row_ver）统一成 ErrReviewConflict。
func reviewErr(id uint64, step string, err error) error {
	if errors.Is(err, repository.ErrReviewConflict) || errors.Is(err, repository.ErrJudgeRowConflict) {
		return fmt.Errorf("service: review task %d %s: %w: %w", id, step, ErrReviewConflict, err)
	}
	return fmt.Errorf("service: review task %d %s: %w", id, step, err)
}

func equalScore(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// decodeJSON 解码为通用值（数字保留原文，避免大整数失真）。
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return v, nil
}

// hasPII 报告 JSON 值里是否有任何键 / 字符串 / 数字经 Mask 后会变（手机号 / 身份证 / 账号 / 精确坐标）。
func (s *ReviewService) hasPII(v any) bool {
	switch x := v.(type) {
	case string:
		return s.d.Mask(x) != x
	case json.Number:
		return s.d.Mask(x.String()) != x.String()
	case []any:
		for _, e := range x {
			if s.hasPII(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if s.d.Mask(k) != k || s.hasPII(e) {
				return true
			}
		}
	}
	return false
}
