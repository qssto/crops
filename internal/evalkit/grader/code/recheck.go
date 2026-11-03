package code

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/model"
)

// recheck_returned 用到的 outbox / 设备调用约定（P1-L4-A；与 repository.TopicOrderDispatch / TopicNotifySend、
// service 通知 dedup_key、env.MethodDryRun 一致，打分器不 import 被测实现）。
const (
	topicOrderDispatch   = "order.dispatch"
	topicNotifySend      = "notify.send"
	notifyReturnedPrefix = "notify:RECHECK_RETURNED:"
	outboxDone           = "DONE"
	deviceMethodDryRun   = "DryRun"
)

// errNoOutbox 表示断言 expect.dispatch 但 trial 未记录 outbox（无 trial 库或无作业单），无法判定。
var errNoOutbox = errors.New("outbox not recorded")

// errNoChecks 表示断言 recheck_failed_rules 但 trial 未记录作业单约束检查，无法判定。
var errNoChecks = errors.New("constraint checks not recorded")

// RecheckReturned 是 recheck_returned（P1-L4-A）：断言 expect.dispatch 的样本经 outbox 排空后——
//   - recheck_returned=true：有 APPROVED → PENDING_APPROVAL(RECHECK_FAILED) 迁移；设备下发数不超过退回之后的重审通过次数
//     （被拦截的那次未下发）；notify.send(notify:RECHECK_RETURNED:…) 已入 outbox；
//     未重审（fixtures.dispatch.reapprove 为空）时最新票是 OPEN 的新票（ticket_seq > 1）；
//   - recheck_returned=false：无 RECHECK_FAILED 迁移；
//   - device_dispatches 给定时 DryRun + Dispatch 调用数须相等（重复投递计数 = 1）；
//   - recheck_failed_rules 给定时每个 rule_id 都有 PRE_DISPATCH 且 passed=false 的约束检查；
//   - order.dispatch 消息全部 DONE（无卡住 / DEAD）。
//
// 未断言 expect.dispatch 的样本直接通过；断言了但缺观测返回 grader_error。
type RecheckReturned struct{}

var _ grader.Grader = RecheckReturned{}

// Name 返回打分器名。
func (RecheckReturned) Name() string { return "recheck_returned" }

// RedLine 返回 true（复检不通过必须拦截下发，P1-L4-A 红线）。
func (RecheckReturned) RedLine() bool { return true }

// Grade 打分。
func (g RecheckReturned) Grade(_ context.Context, t *grader.Trial) (grader.Score, error) {
	if err := checkTrial(g.Name(), t); err != nil {
		return grader.Score{}, err
	}
	exp := t.Sample.Expect.Dispatch
	if exp == nil {
		return grader.NotApplicable("no expect.dispatch"), nil
	}
	switch {
	case t.OrderTransitions == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoTransitions)
	case t.Approval == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoApproval)
	case t.DeviceTargets == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoDevice)
	case t.Outbox == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoOutbox)
	case len(exp.RecheckFailedRules) > 0 && t.ConstraintChecks == nil:
		return grader.Score{}, fmt.Errorf("%s: %s: %w", g.Name(), t.Sample.ID, errNoChecks)
	}
	var fails []string
	returned := returnedAt(t.OrderTransitions)
	if exp.RecheckReturned {
		fails = append(fails, checkReturned(t, returned)...)
	} else if returned >= 0 {
		fails = append(fails, "order returned to PENDING_APPROVAL(RECHECK_FAILED), want no return")
	}
	fails = append(fails, missingFailedRules(t.ConstraintChecks, exp.RecheckFailedRules)...)
	if want := exp.DeviceDispatches; want != nil {
		if got := countDispatches(t.DeviceTargets); got != *want {
			fails = append(fails, fmt.Sprintf("device dispatches %d, want %d", got, *want))
		}
	}
	for _, m := range t.Outbox {
		if m.Topic == topicOrderDispatch && m.Status != outboxDone {
			fails = append(fails, fmt.Sprintf("outbox %s %s, want DONE", m.DedupKey, m.Status))
		}
	}
	if len(fails) > 0 {
		return grader.Score{Evidence: strings.Join(fails, "; ")}, nil
	}
	return grader.Score{Pass: true, Value: 1}, nil
}

// returnedAt 返回首条 APPROVED → PENDING_APPROVAL(RECHECK_FAILED) 迁移的下标，没有返回 -1。
func returnedAt(ts []model.OrderTransition) int {
	for i, tr := range ts {
		if tr.From == model.OrderApproved && tr.To == model.OrderPendingApproval && tr.ReasonCode == model.ReasonRecheckFailed {
			return i
		}
	}
	return -1
}

// missingFailedRules 列出 want 中没有对应「PRE_DISPATCH 且 passed=false」约束检查的 rule_id（P1-L8-A 审查修复：
// 只看迁移无法区分是电量还是载药量拦下的，样本须靠观测归因而不是靠 fixture 只让一项不满足）。
func missingFailedRules(checks []model.ConstraintCheck, want []string) []string {
	failed := make(map[string]bool, len(checks))
	for _, c := range checks {
		if c.Phase == model.PhasePreDispatch && !c.Passed {
			failed[c.RuleID] = true
		}
	}
	var out []string
	for _, id := range want {
		if !failed[id] {
			out = append(out, fmt.Sprintf("no failed %s check for rule %s", model.PhasePreDispatch, id))
		}
	}
	return out
}

// checkReturned 检查退回重审的观测：迁移、拦截下发、通知入 outbox、未重审时新票 OPEN。
func checkReturned(t *grader.Trial, returned int) []string {
	if returned < 0 {
		return []string{"no APPROVED -> PENDING_APPROVAL(RECHECK_FAILED) transition"}
	}
	var fails []string
	reapproved := 0
	for _, tr := range t.OrderTransitions[returned+1:] {
		if tr.To == model.OrderApproved {
			reapproved++
		}
	}
	if got := countDispatches(t.DeviceTargets); got > reapproved {
		fails = append(fails, fmt.Sprintf("device dispatched %d time(s) with %d re-approval(s) after return: blocked dispatch reached device",
			got, reapproved))
	}
	notified := false
	for _, m := range t.Outbox {
		notified = notified || (m.Topic == topicNotifySend && strings.HasPrefix(m.DedupKey, notifyReturnedPrefix))
	}
	if !notified {
		fails = append(fails, "no notify.send "+notifyReturnedPrefix+"* in outbox")
	}
	if t.Sample.Fixtures.Dispatch != nil && t.Sample.Fixtures.Dispatch.Reapprove != "" {
		return fails
	}
	switch tk := t.Approval.Ticket; {
	case tk == nil:
		fails = append(fails, "no reopened ticket")
	case tk.TicketSeq < 2 || tk.Status != model.TicketOpen:
		fails = append(fails, fmt.Sprintf("latest ticket %d seq %d status %s, want reopened (seq > 1) OPEN",
			tk.TicketID, tk.TicketSeq, tk.Status))
	}
	return fails
}

// countDispatches 统计到达设备 Adapter 的下发（DryRun 与 Dispatch；查状态 / 中止不计）。
func countDispatches(ds []grader.DeviceTarget) int {
	n := 0
	for _, d := range ds {
		if d.Method == deviceMethodDryRun || d.Method == model.DeviceMethodDispatch {
			n++
		}
	}
	return n
}
