package env

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// fixtures.approver 取值（评测设计方案 §4：approve / reject / timeout / approve_after: <时长>；不用 LLM 模拟审批人）。
const (
	ApproverApprove     = "approve"
	ApproverReject      = "reject"
	ApproverTimeout     = "timeout"
	approveAfterPrefix  = "approve_after:"
	timeoutPastExpiry   = time.Second // timeout 把时钟拨到 expires_at 之后的余量
	scriptRequestPrefix = "script"
)

var errBadApprover = errors.New("env: bad fixtures.approver")

// ApproverScript 是解析后的 fixtures.approver。After > 0 只与 approve 组合（approve_after）。
type ApproverScript struct {
	Action string
	After  time.Duration
}

// ParseApproverScript 解析 fixtures.approver：approve / reject / timeout / approve_after:<Go 时长，如 90m>。
func ParseApproverScript(v string) (ApproverScript, error) {
	switch v {
	case ApproverApprove, ApproverReject, ApproverTimeout:
		return ApproverScript{Action: v}, nil
	}
	rest, ok := strings.CutPrefix(v, approveAfterPrefix)
	if !ok {
		return ApproverScript{}, fmt.Errorf("%w: %q", errBadApprover, v)
	}
	d, err := time.ParseDuration(strings.TrimSpace(rest))
	if err != nil || d <= 0 {
		return ApproverScript{}, fmt.Errorf("%w: %q: duration must be positive", errBadApprover, v)
	}
	return ApproverScript{Action: ApproverApprove, After: d}, nil
}

// Ticket 是脚本审批人要处理的一张 OPEN 票。
type Ticket struct {
	ID        uint64
	Digest    string
	ExpiresAt time.Time
}

// Identity 是审批人身份（与 configs/auth.yaml 授权一致）。
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
type ApproverDeps struct {
	Clock   *FrozenClock
	Decider Decider
	Sweeper Sweeper
	First   Identity
	Second  Identity
}

// ScriptedApprover 按脚本处理票据：approve（HIGH 票第一审批人通过后由 Second 二次确认）、reject、
// timeout（拨钟越过 expires_at 后跑一轮扫描，作业单转 EXPIRED）、approve_after（先拨钟再通过）。
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
	return &ScriptedApprover{script: script, d: d}, nil
}

// Act 对农场 farm 内的一张票执行脚本，返回票据是否最终通过。approve_after 拨钟后票已过期时，
// Decide 会同事务收尾为 EXPIRED，这正是脚本要构造的结果，不作为错误返回。
func (a *ScriptedApprover) Act(ctx context.Context, farm model.Scope, t Ticket) (approved bool, err error) {
	switch a.script.Action {
	case ApproverTimeout:
		a.d.Clock.Set(t.ExpiresAt.Add(timeoutPastExpiry))
		if err := a.d.Sweeper.RunOnce(ctx); err != nil {
			return false, fmt.Errorf("env: approver timeout ticket %d: %w", t.ID, err)
		}
		return false, nil
	case ApproverReject:
		_, err := a.decide(ctx, farm, a.d.First, t, service.DecisionReject)
		return false, err
	case ApproverApprove:
		if a.script.After > 0 {
			a.d.Clock.Advance(a.script.After)
		}
		res, err := a.decide(ctx, farm, a.d.First, t, service.DecisionApprove)
		if errors.Is(err, service.ErrTicketExpired) && a.script.After > 0 {
			return false, nil
		}
		if err == nil && res.AwaitingConfirm {
			res, err = a.decide(ctx, farm, a.d.Second, t, service.DecisionApprove)
		}
		return err == nil && res.Status == model.TicketApproved, err
	default:
		return false, fmt.Errorf("%w: action %q", errBadApprover, a.script.Action)
	}
}

func (a *ScriptedApprover) decide(ctx context.Context, farm model.Scope, who Identity, t Ticket,
	decision string) (service.DecideResult, error) {
	scope := model.Scope{TenantID: farm.TenantID, FarmID: farm.FarmID, UserID: who.UserID, Roles: who.Roles}
	in := service.DecideInput{TicketID: t.ID, Decision: decision, SnapshotDigest: t.Digest,
		RequestID: fmt.Sprintf("%s-%d-%d-%s", scriptRequestPrefix, t.ID, who.UserID, decision)}
	res, err := a.d.Decider.Decide(ctx, scope, in)
	if err != nil {
		return res, fmt.Errorf("env: approver %s ticket %d by %d: %w", decision, t.ID, who.UserID, err)
	}
	return res, nil
}
