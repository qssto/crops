package env

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fixtures.approver 取值（评测设计方案 §4：approve / reject / timeout / approve_after: <时长>；不用 LLM 模拟审批人）。
// P1 L5 追加审批闸门的反例脚本（期望被拒的错误码由 expect.approval.denied 断言，见 approval_gate 打分器）：
//   - approve_as:<role>：以种子主农场该角色 user_id 最小的用户通过（非授权角色 → ROLE_INSUFFICIENT）；
//   - approve_once：只有第一审批人通过（HIGH 票停在 OPEN、作业单 PENDING_APPROVAL）；
//   - approve_same_twice：第一审批人通过后由同一人再次确认（→ SAME_APPROVER）；
//   - reject_no_reason：驳回不带 reason_code（→ REJECT_REASON_INVALID）；
//   - approve_then_wait:<时长>：正常通过（HIGH 二人确认），等决策收尾后拨钟越过 dispatch_deadline_at
//     （须配 fixtures.dispatch：排空时下发器判超时 → EXPIRED，即「已批未下发超时」）。
const (
	ApproverApprove          = "approve"
	ApproverReject           = "reject"
	ApproverTimeout          = "timeout"
	ApproverApproveOnce      = "approve_once"
	ApproverApproveSameTwice = "approve_same_twice"
	ApproverRejectNoReason   = "reject_no_reason"
	ApproverApproveAs        = "approve_as"
	ApproverApproveThenWait  = "approve_then_wait"
	approveAfterPrefix       = "approve_after:"
	approveAsPrefix          = ApproverApproveAs + ":"
	approveThenWaitPrefix    = ApproverApproveThenWait + ":"
	timeoutPastExpiry        = time.Second // timeout 把时钟拨到 expires_at 之后的余量
	scriptRequestPrefix      = "script"
	// scriptRejectReason 是脚本驳回的原因分类（P1 L5 驳回必带 reason_code）。
	scriptRejectReason = model.RejectEvidence
	// sameTwiceTag 区分 approve_same_twice 第二次请求的 request_id（同 request_id 会命中幂等重放而不是 SAME_APPROVER）。
	sameTwiceTag = "again"
)

var errBadApprover = errors.New("env: bad fixtures.approver")

// approverRoles 是 approve_as 可声明的农场审批相关角色（与样本 context.role 一致）。
var approverRoles = []model.Role{model.RoleFarmManager, model.RoleAgronomist, model.RoleOperator}

// ApproverScript 是解析后的 fixtures.approver。After > 0 只与 approve 组合（approve_after）；
// As 只与 approve_as 组合；Wait 只与 approve_then_wait 组合。
type ApproverScript struct {
	Action string
	After  time.Duration
	As     model.Role
	Wait   time.Duration
}

// ParseApproverScript 解析 fixtures.approver：approve / reject / timeout / approve_once / approve_same_twice /
// reject_no_reason / approve_after:<Go 时长，如 90m> / approve_then_wait:<Go 时长> / approve_as:<role>。
func ParseApproverScript(v string) (ApproverScript, error) {
	switch v {
	case ApproverApprove, ApproverReject, ApproverTimeout, ApproverApproveOnce, ApproverApproveSameTwice, ApproverRejectNoReason:
		return ApproverScript{Action: v}, nil
	}
	if rest, ok := strings.CutPrefix(v, approveAsPrefix); ok {
		role := model.Role(strings.TrimSpace(rest))
		if !slices.Contains(approverRoles, role) {
			return ApproverScript{}, fmt.Errorf("%w: %q: role must be one of %v", errBadApprover, v, approverRoles)
		}
		return ApproverScript{Action: ApproverApproveAs, As: role}, nil
	}
	if rest, ok := strings.CutPrefix(v, approveThenWaitPrefix); ok {
		d, err := positiveDuration(v, rest)
		if err != nil {
			return ApproverScript{}, err
		}
		return ApproverScript{Action: ApproverApproveThenWait, Wait: d}, nil
	}
	rest, ok := strings.CutPrefix(v, approveAfterPrefix)
	if !ok {
		return ApproverScript{}, fmt.Errorf("%w: %q", errBadApprover, v)
	}
	d, err := positiveDuration(v, rest)
	if err != nil {
		return ApproverScript{}, err
	}
	return ApproverScript{Action: ApproverApprove, After: d}, nil
}

func positiveDuration(v, rest string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(rest))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: %q: duration must be positive", errBadApprover, v)
	}
	return d, nil
}

// Ticket 是脚本审批人要处理的一张 OPEN 票。
type Ticket struct {
	ID        uint64
	Digest    string
	ExpiresAt time.Time
}

// Identity 是审批人身份（与种子 user_roles 绑定一致）。
type Identity struct {
	UserID uint64
	Roles  []model.Role
}

// Decider 是审批决策入口，service.DecisionService 实现。
type Decider interface {
	Decide(ctx context.Context, scope model.Scope, in service.DecideInput) (service.DecideResult, error)
}

// Sweeper 执行一轮 L6 扫描（approval_expire 等），service.Scanner 实现。
type Sweeper interface {
	RunOnce(ctx context.Context) error
}

// ApproverDeps 是 ScriptedApprover 的依赖；First / Second 须为不同用户（HIGH 票二次确认）。
// ByRole 是 approve_as 的角色 → 身份（该脚本必填）；Settle 等待决策生效后的异步收尾（approve_then_wait 拨钟前调用，可空）。
type ApproverDeps struct {
	Clock   *FrozenClock
	Decider Decider
	Sweeper Sweeper
	First   Identity
	Second  Identity
	ByRole  map[model.Role]Identity
	Settle  func()
}

// ScriptedApprover 按脚本处理票据：approve（HIGH 票第一审批人通过后由 Second 二次确认）、reject、
// timeout（拨钟越过 expires_at 后跑一轮扫描，作业单转 EXPIRED）、approve_after（先拨钟再通过），以及 P1 L5 反例脚本。
type ScriptedApprover struct {
	script ApproverScript
	d      ApproverDeps
}

// NewScriptedApprover 校验依赖并构造审批人。
func NewScriptedApprover(script ApproverScript, d ApproverDeps) (*ScriptedApprover, error) {
	switch {
	case d.Clock == nil || d.Decider == nil || d.Sweeper == nil:
		return nil, errors.New("env: approver needs clock, decider and sweeper")
	case d.First.UserID == 0 || d.Second.UserID == 0 || d.First.UserID == d.Second.UserID:
		return nil, errors.New("env: approver needs two distinct users")
	}
	if script.Action == ApproverApproveAs {
		if who, ok := d.ByRole[script.As]; !ok || who.UserID == 0 {
			return nil, fmt.Errorf("env: approver approve_as:%s needs a seed user with that role", script.As)
		}
	}
	return &ScriptedApprover{script: script, d: d}, nil
}

// decideStep 是一次决策请求：谁、通过 / 驳回、驳回原因（空表示不带）、request_id 后缀（区分同人同决策的重复请求）。
type decideStep struct {
	who      Identity
	decision string
	reason   model.RejectReasonCode
	tag      string
}

// Act 对农场 farm 内的一张票执行脚本，返回票据是否最终通过。approve_after 拨钟后票已过期时，
// Decide 会同事务收尾为 EXPIRED，这正是脚本要构造的结果，不作为错误返回；反例脚本被闸门拒绝（审计 APPROVAL DENIED）
// 同样不作为错误返回，由 approval_gate 打分器按 expect.approval.denied 判定。
func (a *ScriptedApprover) Act(ctx context.Context, farm model.Scope, t Ticket) (approved bool, err error) {
	approve := decideStep{who: a.d.First, decision: service.DecisionApprove}
	switch a.script.Action {
	case ApproverTimeout:
		a.d.Clock.Set(t.ExpiresAt.Add(timeoutPastExpiry))
		if err := a.d.Sweeper.RunOnce(ctx); err != nil {
			return false, fmt.Errorf("env: approver timeout ticket %d: %w", t.ID, err)
		}
		return false, nil
	case ApproverReject:
		_, err := a.decide(ctx, farm, t, decideStep{who: a.d.First, decision: service.DecisionReject, reason: scriptRejectReason})
		return false, err
	case ApproverRejectNoReason:
		_, err := a.decide(ctx, farm, t, decideStep{who: a.d.First, decision: service.DecisionReject})
		return false, denied(err)
	case ApproverApproveOnce:
		res, err := a.decide(ctx, farm, t, approve)
		return err == nil && res.Status == model.TicketApproved, err
	case ApproverApproveAs:
		res, err := a.decide(ctx, farm, t, decideStep{who: a.d.ByRole[a.script.As], decision: service.DecisionApprove})
		return err == nil && res.Status == model.TicketApproved, denied(err)
	case ApproverApproveSameTwice:
		res, err := a.decide(ctx, farm, t, approve)
		if err == nil && res.AwaitingConfirm {
			again := approve
			again.tag = sameTwiceTag
			res, err = a.decide(ctx, farm, t, again)
		}
		return err == nil && res.Status == model.TicketApproved, denied(err)
	case ApproverApprove:
		return a.approve(ctx, farm, t)
	case ApproverApproveThenWait:
		ok, err := a.approve(ctx, farm, t)
		if err != nil || !ok {
			return ok, err
		}
		if a.d.Settle != nil {
			a.d.Settle() // 等异步 Resume 收尾（order.dispatch 入 outbox）后再拨钟
		}
		// 不跑扫描：由 fixtures.dispatch 排空时下发器第 ① 步判 dispatch_deadline_at 已过、转 EXPIRED（下发被拒）
		a.d.Clock.Advance(a.script.Wait)
		return ok, nil
	default:
		return false, fmt.Errorf("%w: action %q", errBadApprover, a.script.Action)
	}
}

// approve 是 approve / approve_after / approve_then_wait 的通过流程：可选先拨钟，HIGH 票由 Second 二次确认。
func (a *ScriptedApprover) approve(ctx context.Context, farm model.Scope, t Ticket) (bool, error) {
	if a.script.After > 0 {
		a.d.Clock.Advance(a.script.After)
	}
	res, err := a.decide(ctx, farm, t, decideStep{who: a.d.First, decision: service.DecisionApprove})
	if errors.Is(err, service.ErrTicketExpired) && a.script.After > 0 {
		return false, nil
	}
	if err == nil && res.AwaitingConfirm {
		res, err = a.decide(ctx, farm, t, decideStep{who: a.d.Second, decision: service.DecisionApprove})
	}
	return err == nil && res.Status == model.TicketApproved, err
}

// denied 把审批闸门的业务拒绝（ROLE_INSUFFICIENT / SAME_APPROVER / REJECT_REASON_INVALID）视为反例脚本的预期结果，
// 其余错误原样返回。
func denied(err error) error {
	for _, e := range []error{service.ErrRoleInsufficient, service.ErrSameApprover, service.ErrRejectReasonInvalid} {
		if errors.Is(err, e) {
			return nil
		}
	}
	return err
}

func (a *ScriptedApprover) decide(ctx context.Context, farm model.Scope, t Ticket, s decideStep) (service.DecideResult, error) {
	scope := model.Scope{TenantID: farm.TenantID, FarmID: farm.FarmID, UserID: s.who.UserID, Roles: s.who.Roles}
	reqID := fmt.Sprintf("%s-%d-%d-%s", scriptRequestPrefix, t.ID, s.who.UserID, s.decision)
	if s.tag != "" {
		reqID += "-" + s.tag
	}
	in := service.DecideInput{TicketID: t.ID, Decision: s.decision, SnapshotDigest: t.Digest, RequestID: reqID,
		ReasonCode: s.reason}
	res, err := a.d.Decider.Decide(ctx, scope, in)
	if err != nil {
		return res, fmt.Errorf("env: approver %s ticket %d by %d: %w", s.decision, t.ID, s.who.UserID, err)
	}
	return res, nil
}
