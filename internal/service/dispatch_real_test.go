package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// newRealFixture 是 REAL 单、enable_robot_dispatch 开、设备支持 IdemKey 且受理（job-1）的下发夹具。
func newRealFixture(t *testing.T, status model.WorkOrderStatus) *dispatchFixture {
	t.Helper()
	f := newDispatchFixture(t, status, false)
	f.store.order.DispatchMode = string(model.DispatchReal)
	f.flags.robot = true
	return f
}

// realMsg 是第 attempt 次 order.dispatch（attempt=1 时 payload 不带 attempt，与 DecisionRepo 写法一致）。
func realMsg(attempt int) repository.OutboxMessage {
	payload := okDispatchPayload
	if attempt > 1 {
		payload = fmt.Sprintf(`{"order_id":3,"ticket_id":7,"run_id":5,"attempt":%d}`, attempt)
	}
	m := dispatchMsg(payload)
	m.DedupKey = DispatchIdemKey(3, 7, attempt)
	return m
}

// seedAttempt 预置一条尝试（崩溃续跑 / 重试场景）。
func (f *dispatchFixture) seedAttempt(attempt int, status model.AttemptStatus, kind model.FailureKind, job string) {
	f.store.attempts = append(f.store.attempts, model.DispatchAttempt{ID: uint64(len(f.store.attempts) + 1), TenantID: 1,
		FarmID: 1, OrderID: 3, TicketID: 7, Attempt: attempt, DeviceType: "drone", DeviceID: "uav-1",
		IdemKey: DispatchIdemKey(3, 7, attempt), DeviceJobID: job, Mode: model.DispatchReal, Status: status,
		FailureKind: kind, CommandRawID: 600, RowVer: 1})
}

// realPhases 返回 DISPATCHED 事件 phase:device_touched 序列，并校验 REAL payload 字段。
func (f *dispatchFixture) realPhases(t *testing.T) string {
	t.Helper()
	var out []string
	for _, e := range f.events.events {
		if e.Type != model.EventDispatched {
			continue
		}
		var p model.RealDispatchedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.DryRun || p.DispatchMode != model.DispatchReal || p.DeviceID != "uav-1" || p.DeviceType != "drone" {
			t.Errorf("REAL DISPATCHED payload = %+v", p)
		}
		out = append(out, fmt.Sprintf("%s:%t", p.Phase, p.DeviceTouched))
	}
	return fmt.Sprint(out)
}

// REAL 首次下发：③ 绑定 → ④ 复检 → Archive → SENT（flag_read_at / command_raw_id / receipt_due_at）→ APPROVED → DISPATCHED →
// idem:dispatch 7d → DispatchArchived(idem_key) → 回填 job（ACCEPTED，顺延 receipt_due_at）。不走 DryRun。
func TestDispatchReal_Accepted(t *testing.T) {
	f := newRealFixture(t, model.OrderApproved)
	if err := f.svc.Dispatch(context.Background(), realMsg(1)); err != nil {
		t.Fatal(err)
	}
	if f.store.order.Status != string(model.OrderDispatched) || f.dev.dryRuns != 0 || f.real.dispatches != 1 ||
		f.real.archives != 1 || f.pre.calls != 1 {
		t.Fatalf("status=%s dryRuns=%d dispatches=%d archives=%d prechecks=%d", f.store.order.Status, f.dev.dryRuns,
			f.real.dispatches, f.real.archives, f.pre.calls)
	}
	idem := DispatchIdemKey(3, 7, 1)
	if len(f.store.attempts) != 1 {
		t.Fatalf("attempts = %+v", f.store.attempts)
	}
	a := f.store.attempts[0]
	if a.Status != model.AttemptAccepted || a.DeviceJobID != "job-1" || a.IdemKey != idem || a.DeviceID != "uav-1" ||
		a.CommandRawID != 600 || a.Attempt != 1 || a.TicketID != 7 || !a.FlagReadAt.Equal(dispatchNow) ||
		!a.ReceiptDueAt.Equal(dispatchNow.Add(30*time.Minute)) {
		t.Errorf("attempt = %+v", a)
	}
	if f.real.keys[0] != idem || f.real.rawIDs[0] != 600 {
		t.Errorf("DispatchArchived key=%s raw=%d", f.real.keys[0], f.real.rawIDs[0])
	}
	if v := f.idem.m[f.deps.Keys.IdemDispatch(3, 7, 1)]; v != idem {
		t.Errorf("idem:dispatch = %q, want %q", v, idem)
	}
	if got := f.realPhases(t); got != "[INTENT:false RESULT:true]" {
		t.Errorf("DISPATCHED = %s", got)
	}
	if len(f.store.transitions) != 1 || f.store.transitions[0].To != model.OrderDispatched {
		t.Errorf("transitions = %+v", f.store.transitions)
	}
}

// 重复投递（回执前 / 受理后）：按 idem_key 查到尝试即 DONE，设备调用计数 = 1（应失败：第二次又调设备）。
func TestDispatchReal_DuplicateDeliveryCallsDeviceOnce(t *testing.T) {
	f := newRealFixture(t, model.OrderApproved)
	for i := 0; i < 3; i++ {
		if err := f.svc.Dispatch(context.Background(), realMsg(1)); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if f.real.dispatches != 1 || f.real.archives != 1 || len(f.store.attempts) != 1 || f.pre.calls != 1 {
		t.Fatalf("dispatches=%d archives=%d attempts=%d prechecks=%d, want 1/1/1/1", f.real.dispatches, f.real.archives,
			len(f.store.attempts), f.pre.calls)
	}
}

// ④ PRE_DISPATCH 约束不通过（红线）：首次 → Reopen 退回，绝不 Archive / 调设备 / 写 SENT / DryRun；
// 与结论变化同时出现时仍走退回、不 supersede（应失败：照常下发，或走 Changed 取消）。
func TestDispatch_BlockedReopensWithoutDispatch(t *testing.T) {
	blocked := PreDispatchResult{Blocked: []string{"kb#wind"}, Checks: []model.ConstraintCheck{
		{Phase: model.PhasePreDispatch, RuleID: "kb#wind", Passed: false}}}
	for name, tc := range map[string]struct {
		real    bool
		changed bool
	}{"real": {real: true}, "dry-run": {}, "blocked beats changed": {real: true, changed: true}} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t, model.OrderApproved, false)
			if tc.real {
				f = newRealFixture(t, model.OrderApproved)
			}
			f.pre.res = blocked
			f.pre.res.Changed, f.pre.res.NewRunID = tc.changed, 0
			if err := f.svc.Dispatch(context.Background(), realMsg(1)); err != nil {
				t.Fatal(err)
			}
			if f.reopener.prepare != 1 || f.reopener.reopen != 1 || f.store.order.Status != string(model.OrderPendingApproval) {
				t.Fatalf("prepare=%d reopen=%d status=%s", f.reopener.prepare, f.reopener.reopen, f.store.order.Status)
			}
			if f.real.archives+f.real.dispatches+f.dev.dryRuns != 0 || len(f.store.attempts) != 0 || f.store.superseded != [2]uint64{} {
				t.Fatalf("dispatched despite block: archives=%d dispatches=%d dryRuns=%d attempts=%d superseded=%v",
					f.real.archives, f.real.dispatches, f.dev.dryRuns, len(f.store.attempts), f.store.superseded)
			}
			got := f.reopener.got
			if got.TicketID != 7 || fmt.Sprint(got.RuleIDs) != "[kb#wind]" || len(got.Order.ConstraintChecks) != 1 ||
				payloadDeviceID(got.Order.Payload) != "uav-1" || got.Card.SelectedProduct != "P1" {
				t.Errorf("reopen request = %+v", got)
			}
			if len(f.store.checks) != 1 || f.store.checkEpoch != 3 {
				t.Errorf("PRE_DISPATCH checks = %+v epoch %d", f.store.checks, f.store.checkEpoch)
			}
		})
	}
}

// Reopen 失败（如旧票 row_ver 冲突）：返回错误，单不动、不下发。
func TestDispatch_ReopenFailureKeepsOrder(t *testing.T) {
	f := newRealFixture(t, model.OrderApproved)
	f.pre.res = PreDispatchResult{Blocked: []string{"kb#wind"}}
	f.reopener.err = model.ErrConcurrentUpdate
	if err := f.svc.Dispatch(context.Background(), realMsg(1)); !errors.Is(err, model.ErrConcurrentUpdate) {
		t.Fatalf("err = %v", err)
	}
	if f.store.order.Status != string(model.OrderApproved) || f.real.dispatches != 0 || f.reopener.reopen != 0 {
		t.Fatalf("status=%s dispatches=%d reopen=%d", f.store.order.Status, f.real.dispatches, f.reopener.reopen)
	}
}

// ③ 设备不可用 / 跨农场（红线）：选不到设备 → 推后（ErrOutboxDeferred）、写 DEVICE_UNAVAILABLE、不复检不调设备、单不动；
// 绑定后 device_id 丢失 → 不下发（应失败：无 device_id 照常调设备）。
func TestDispatch_DeviceUnavailableDefers(t *testing.T) {
	for name, err := range map[string]error{
		"unavailable":  fmt.Errorf("%w: all offline", ErrDeviceUnavailable),
		"not bindable": fmt.Errorf("%w: manual", ErrDeviceNotBindable),
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderApproved)
			f.picker.err = err
			if got := f.svc.Dispatch(context.Background(), realMsg(1)); !errors.Is(got, ErrOutboxDeferred) {
				t.Fatalf("err = %v, want ErrOutboxDeferred", got)
			}
			if f.real.dispatches+f.real.archives != 0 || f.pre.calls != 0 || f.store.order.Status != string(model.OrderApproved) ||
				payloadDeviceID(f.store.order.Payload) != "" {
				t.Fatalf("dispatches=%d prechecks=%d status=%s payload=%s", f.real.dispatches, f.pre.calls,
					f.store.order.Status, f.store.order.Payload)
			}
			if len(f.audit.recs) != 1 || f.audit.recs[0].Action != string(model.AuditDeviceUnavailable) ||
				f.audit.recs[0].Result != model.AuditResultDenied || f.audit.recs[0].Target != "order:3" {
				t.Fatalf("audit = %+v", f.audit.recs)
			}
		})
	}
	t.Run("bound device lost", func(t *testing.T) {
		f := newRealFixture(t, model.OrderApproved)
		f.store.dropBind = true
		if err := f.svc.Dispatch(context.Background(), realMsg(1)); !errors.Is(err, ErrDispatchState) {
			t.Fatalf("err = %v, want ErrDispatchState", err)
		}
		if f.real.dispatches+f.real.archives != 0 || len(f.store.attempts) != 0 {
			t.Fatalf("dispatches=%d attempts=%d", f.real.dispatches, len(f.store.attempts))
		}
	})
}

// P1-L4 审查修复：选不到设备的推后有上限。单无 dispatch_deadline_at 时自消息入队起超 DeviceWaitMax（2h）→ 放弃下发：
// 首次 APPROVED → EXPIRED、重试 DISPATCHED → FAILED，reason DEVICE_UNAVAILABLE，审计 DEVICE_UNAVAILABLE(FAILED)，返回 nil（outbox DONE）；
// 未超限 / 有时效且未到 / 无入队时刻 → 照旧推后。应失败：无时效的单每 5s 推后一次、永不终止。
func TestDispatch_DeviceUnavailableGivesUp(t *testing.T) {
	tests := []struct {
		name       string
		attempt    int
		deadline   bool
		waited     time.Duration // 0 表示消息无 created_at
		wantDefer  bool
		wantStatus model.WorkOrderStatus
	}{
		{name: "first attempt, no deadline, over max", attempt: 1, waited: 2 * time.Hour, wantStatus: model.OrderExpired},
		{name: "retry, no deadline, over max", attempt: 2, waited: 3 * time.Hour, wantStatus: model.OrderFailed},
		{name: "no deadline, within max", attempt: 1, waited: 2*time.Hour - time.Second, wantDefer: true,
			wantStatus: model.OrderApproved},
		{name: "deadline not reached governs", attempt: 1, deadline: true, waited: 5 * time.Hour, wantDefer: true,
			wantStatus: model.OrderApproved},
		{name: "no created_at", attempt: 1, wantDefer: true, wantStatus: model.OrderApproved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := model.OrderApproved
			if tt.attempt > 1 {
				status = model.OrderDispatched
			}
			f := newRealFixture(t, status)
			if !tt.deadline {
				f.store.order.DispatchDeadlineAt = nil
			}
			f.picker.err = fmt.Errorf("%w: all offline", ErrDeviceUnavailable)
			msg := realMsg(tt.attempt)
			if tt.waited > 0 {
				msg.CreatedAt = dispatchNow.Add(-tt.waited)
			}
			err := f.svc.Dispatch(context.Background(), msg)
			if tt.wantDefer != errors.Is(err, ErrOutboxDeferred) || (!tt.wantDefer && err != nil) {
				t.Fatalf("err = %v, want deferred %t", err, tt.wantDefer)
			}
			if f.store.order.Status != string(tt.wantStatus) || f.real.dispatches+f.real.archives != 0 || f.pre.calls != 0 {
				t.Fatalf("status=%s dispatches=%d prechecks=%d", f.store.order.Status, f.real.dispatches, f.pre.calls)
			}
			wantResult := model.AuditResultDenied
			if !tt.wantDefer {
				wantResult = model.AuditResultFailed
				if n := len(f.store.transitions); n != 1 || f.store.transitions[0].ReasonCode != model.ReasonDeviceUnavailable {
					t.Fatalf("transitions = %+v", f.store.transitions)
				}
			} else if len(f.store.transitions) != 0 {
				t.Fatalf("deferred but transitions = %+v", f.store.transitions)
			}
			if len(f.audit.recs) != 1 || f.audit.recs[0].Action != string(model.AuditDeviceUnavailable) ||
				f.audit.recs[0].Result != wantResult {
				t.Fatalf("audit = %+v, want one DEVICE_UNAVAILABLE/%s", f.audit.recs, wantResult)
			}
		})
	}
}

// DRY_RUN 单：manual 等无设备类别的类型跳过绑定照常 dry-run；有类别但不可用同样推后。
func TestDispatch_DryRunDeviceBinding(t *testing.T) {
	f := newDispatchFixture(t, model.OrderApproved, false)
	f.picker.err = ErrDeviceNotBindable
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); err != nil {
		t.Fatal(err)
	}
	if f.dev.dryRuns != 1 || f.store.order.Status != string(model.OrderDone) {
		t.Fatalf("dryRuns=%d status=%s", f.dev.dryRuns, f.store.order.Status)
	}
	f = newDispatchFixture(t, model.OrderApproved, false)
	f.picker.err = ErrDeviceUnavailable
	if err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload)); !errors.Is(err, ErrOutboxDeferred) {
		t.Fatalf("err = %v", err)
	}
	if f.dev.dryRuns != 0 || f.pre.calls != 0 {
		t.Fatalf("dryRuns=%d prechecks=%d", f.dev.dryRuns, f.pre.calls)
	}
}

// dispatched 把单置为已下发（DISPATCHED、已绑 uav-1），供续跑 / 重试用例。
func (f *dispatchFixture) dispatched() {
	f.store.order.Status = string(model.OrderDispatched)
	f.store.order.Payload = json.RawMessage(`{"product":"P1","area_mu":12,"device_id":"uav-1"}`)
}

// SENT 无 device_job_id 的续跑（崩溃在设备调用前后）：
//   - 无 IdemKey 且无 ExternalQuery（红线）→ FAILED(UNKNOWN) + DISPATCH_MANUAL，禁止重发（应失败：又调设备）；
//   - 仅 IdemKey → 带原 idem_key / 原 command_raw_id 重发，不重新 Archive、不复检；
//   - ExternalQuery 查到 → 回填不重发；查询出错 → 返回错误、尝试保持 SENT；
//   - 查无此单：仅 ExternalQuery（红线 §6.5）→ 转人工、下发 0 次（应失败：外部单号索引延迟时重发产生二次作业）；
//     IdemKey + ExternalQuery → 带原键重发 1 次。
func TestDispatchReal_ResumeSent(t *testing.T) {
	idem := DispatchIdemKey(3, 7, 1)
	for name, tc := range map[string]struct {
		caps       adapter.Capabilities
		queryRC    model.DeviceReceipt
		queryErr   error
		wantErr    bool
		dispatches int
		queries    int
		status     model.AttemptStatus
		kind       model.FailureKind
		job        string
	}{
		"no idem no query → manual": {status: model.AttemptFailed, kind: model.FailureUnknown},
		"idem key → resend":         {caps: adapter.Capabilities{IdemKey: true}, dispatches: 1, status: model.AttemptAccepted, job: "job-1"},
		"query found": {caps: adapter.Capabilities{ExternalQuery: true}, queries: 1, status: model.AttemptAccepted, job: "job-9",
			queryRC: model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "job-9"}},
		"query-only not found → manual": {caps: adapter.Capabilities{ExternalQuery: true}, queryErr: adapter.ErrJobNotFound,
			queries: 1, status: model.AttemptFailed, kind: model.FailureUnknown},
		"idem+query not found → resend": {caps: adapter.Capabilities{IdemKey: true, ExternalQuery: true},
			queryErr: adapter.ErrJobNotFound, queries: 1, dispatches: 1, status: model.AttemptAccepted, job: "job-1"},
		"query error keeps SENT": {caps: adapter.Capabilities{ExternalQuery: true}, queryErr: errors.New("platform down"),
			wantErr: true, queries: 1, status: model.AttemptSent},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderDispatched)
			f.dispatched()
			f.seedAttempt(1, model.AttemptSent, "", "")
			f.real.caps, f.real.queryRC, f.real.queryErr = tc.caps, tc.queryRC, tc.queryErr
			err := f.svc.Dispatch(context.Background(), realMsg(1))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if f.real.dispatches != tc.dispatches || f.real.queries != tc.queries || f.real.archives != 0 || f.pre.calls != 0 ||
				f.picker.calls != 0 {
				t.Fatalf("dispatches=%d queries=%d archives=%d prechecks=%d picks=%d", f.real.dispatches, f.real.queries,
					f.real.archives, f.pre.calls, f.picker.calls)
			}
			if tc.dispatches > 0 && (f.real.keys[0] != idem || f.real.rawIDs[0] != 600) {
				t.Errorf("resend key=%s raw=%d, want original", f.real.keys[0], f.real.rawIDs[0])
			}
			a := f.store.attempts[0]
			if len(f.store.attempts) != 1 || a.Status != tc.status || a.FailureKind != tc.kind || a.DeviceJobID != tc.job {
				t.Errorf("attempts = %+v", f.store.attempts)
			}
			manual := tc.kind == model.FailureUnknown
			if manual != (len(f.audit.recs) == 1) || (manual && f.audit.recs[0].Action != string(model.AuditDispatchManual)) {
				t.Errorf("audit = %+v", f.audit.recs)
			}
			if f.store.order.Status != string(model.OrderDispatched) || len(f.store.transitions) != 0 {
				t.Errorf("status=%s transitions=%+v", f.store.order.Status, f.store.transitions)
			}
		})
	}
}

// 已受理 / 已结束的尝试重复投递：直接 DONE，不碰设备、不复检。
func TestDispatchReal_SettledAttemptIsDuplicate(t *testing.T) {
	f := newRealFixture(t, model.OrderDispatched)
	f.dispatched()
	f.seedAttempt(1, model.AttemptAccepted, "", "job-1")
	if err := f.svc.Dispatch(context.Background(), realMsg(1)); err != nil {
		t.Fatal(err)
	}
	if f.real.dispatches+f.real.queries+f.real.archives+f.pre.calls+f.picker.calls != 0 {
		t.Fatalf("dispatches=%d queries=%d prechecks=%d", f.real.dispatches, f.real.queries, f.pre.calls)
	}
}

// 设备返回处置：明确拒绝 → 尝试 FAILED(NOT_EXECUTED) + 单 FAILED(DISPATCH_REJECTED)；无 job 的执行中失败 → 尝试 FAILED 进对账、
// 单保持 DISPATCHED；超时等其余错误 → 返回错误、尝试保持 SENT 不写 RESULT（应失败：把超时当拒绝或当受理）。
func TestDispatchReal_DeviceOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		rc      model.DeviceReceipt
		err     error
		wantErr bool
		order   model.WorkOrderStatus
		status  model.AttemptStatus
		kind    model.FailureKind
		phases  string
	}{
		"rejected error": {err: fmt.Errorf("%w: no quota", adapter.ErrRejected), order: model.OrderFailed,
			status: model.AttemptFailed, kind: model.FailureNotExecuted, phases: "[INTENT:false RESULT:false]"},
		"failed not executed": {rc: model.DeviceReceipt{Status: model.AttemptFailed, FailureKind: model.FailureNotExecuted},
			order: model.OrderFailed, status: model.AttemptFailed, kind: model.FailureNotExecuted,
			phases: "[INTENT:false RESULT:false]"},
		"failed in execution without job": {rc: model.DeviceReceipt{Status: model.AttemptFailed,
			FailureKind: model.FailureInExecution}, order: model.OrderDispatched, status: model.AttemptFailed,
			kind: model.FailureInExecution, phases: "[INTENT:false]"},
		"timeout keeps SENT": {err: context.DeadlineExceeded, wantErr: true, order: model.OrderDispatched,
			status: model.AttemptSent, phases: "[INTENT:false]"},
		"accepted without job": {rc: model.DeviceReceipt{Status: model.AttemptAccepted}, wantErr: true,
			order: model.OrderDispatched, status: model.AttemptSent, phases: "[INTENT:false]"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderApproved)
			f.real.rc, f.real.err = tc.rc, tc.err
			err := f.svc.Dispatch(context.Background(), realMsg(1))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if f.real.dispatches != 1 || f.store.order.Status != string(tc.order) {
				t.Fatalf("dispatches=%d status=%s", f.real.dispatches, f.store.order.Status)
			}
			a := f.store.attempts[0]
			if len(f.store.attempts) != 1 || a.Status != tc.status || a.FailureKind != tc.kind || a.DeviceJobID != "" {
				t.Errorf("attempts = %+v", f.store.attempts)
			}
			if got := f.realPhases(t); got != tc.phases {
				t.Errorf("DISPATCHED = %s, want %s", got, tc.phases)
			}
			last := f.store.transitions[len(f.store.transitions)-1]
			if tc.order == model.OrderFailed && last.ReasonCode != model.ReasonDispatchRejected {
				t.Errorf("transitions = %+v", f.store.transitions)
			}
		})
	}
}

// 重试（attempt=2，单 DISPATCHED）：新写一条尝试（新 idem_key）、不做状态迁移、复检带 Retry；
// 在 ②④ 或时效被拦 → DISPATCHED → FAILED(RETRY_BLOCKED)，不退回重审、不调设备（应失败：重试被拦仍下发或走 Reopen）。
func TestDispatchReal_Retry(t *testing.T) {
	past := dispatchNow.Add(-time.Minute)
	for name, tc := range map[string]struct {
		mutate     func(f *dispatchFixture)
		dispatches int
		blocked    bool
	}{
		"dispatched":     {mutate: func(*dispatchFixture) {}, dispatches: 1},
		"flag off":       {mutate: func(f *dispatchFixture) { f.flags.robot = false }, blocked: true},
		"overdue":        {mutate: func(f *dispatchFixture) { f.store.order.DispatchDeadlineAt = &past }, blocked: true},
		"recheck blocks": {mutate: func(f *dispatchFixture) { f.pre.res.Blocked = []string{"kb#wind"} }, blocked: true},
		"recheck changed": {mutate: func(f *dispatchFixture) {
			f.pre.res.Changed, f.pre.res.NewRunID = true, 9
		}, blocked: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderDispatched)
			f.dispatched()
			f.seedAttempt(1, model.AttemptFailed, model.FailureNotExecuted, "job-0")
			tc.mutate(f)
			if err := f.svc.Dispatch(context.Background(), realMsg(2)); err != nil {
				t.Fatal(err)
			}
			if f.real.dispatches != tc.dispatches || f.reopener.prepare != 0 || f.store.superseded != [2]uint64{} {
				t.Fatalf("dispatches=%d reopen=%d superseded=%v", f.real.dispatches, f.reopener.prepare, f.store.superseded)
			}
			if tc.blocked {
				if f.store.order.Status != string(model.OrderFailed) || len(f.store.transitions) != 1 ||
					f.store.transitions[0].ReasonCode != model.ReasonRetryBlocked || len(f.store.attempts) != 1 {
					t.Fatalf("status=%s transitions=%+v attempts=%d", f.store.order.Status, f.store.transitions,
						len(f.store.attempts))
				}
				return
			}
			a := f.store.attempts[len(f.store.attempts)-1]
			if len(f.store.attempts) != 2 || a.Attempt != 2 || a.IdemKey != DispatchIdemKey(3, 7, 2) ||
				a.Status != model.AttemptAccepted || f.real.keys[0] != a.IdemKey {
				t.Errorf("attempts = %+v keys %v", f.store.attempts, f.real.keys)
			}
			if len(f.store.transitions) != 0 || f.store.order.Status != string(model.OrderDispatched) || !f.pre.got.Retry {
				t.Errorf("transitions=%+v status=%s retry=%t", f.store.transitions, f.store.order.Status, f.pre.got.Retry)
			}
		})
	}
}

// 消息与状态要求：attempt 超上限 / dedup_key 不符 / REAL 缺 dedup_key → ErrBadOutboxPayload；
// attempt=2 而单仍 APPROVED → 丢弃（应失败：把重试当首次下发）。均不调设备。
func TestDispatchReal_MessageRules(t *testing.T) {
	for name, tc := range map[string]struct {
		msg     func() repository.OutboxMessage
		wantBad bool
	}{
		"attempt over max": {msg: func() repository.OutboxMessage { return realMsg(3) }, wantBad: true},
		"dedup mismatch": {msg: func() repository.OutboxMessage {
			m := realMsg(1)
			m.DedupKey = DispatchIdemKey(3, 7, 2)
			return m
		}, wantBad: true},
		"real without dedup": {msg: func() repository.OutboxMessage {
			m := realMsg(1)
			m.DedupKey = ""
			return m
		}, wantBad: true},
		"retry for approved order dropped": {msg: func() repository.OutboxMessage { return realMsg(2) }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderApproved)
			err := f.svc.Dispatch(context.Background(), tc.msg())
			if errors.Is(err, ErrBadOutboxPayload) != tc.wantBad || (!tc.wantBad && err != nil) {
				t.Fatalf("err = %v, wantBad %t", err, tc.wantBad)
			}
			if f.real.dispatches+f.real.archives+f.pre.calls+f.picker.calls != 0 || len(f.store.attempts) != 0 ||
				f.store.order.Status != string(model.OrderApproved) {
				t.Fatalf("dispatches=%d prechecks=%d attempts=%d status=%s", f.real.dispatches, f.pre.calls,
					len(f.store.attempts), f.store.order.Status)
			}
		})
	}
}

// cancelingPrecheck 在复检（锁外）期间把单改成 CANCELED，模拟 L4-B 中止与重试下发并发：admit 之后、持锁之前被中止。
type cancelingPrecheck struct{ *fakePrecheck }

func (c cancelingPrecheck) PreDispatch(ctx context.Context, in PreDispatchInput) (PreDispatchResult, error) {
	c.store.order.Status = string(model.OrderCanceled)
	return c.fakePrecheck.PreDispatch(ctx, in)
}

// P1-L4 合并统一（接缝：L4-B 中止等重试的 NOT_EXECUTED 尝试 → CANCELED）：CANCELED 单上的 attempt=2 消息直接丢弃——
// 不调 Adapter、不写尝试、不改状态（不转 FAILED）、返回 nil（outbox DONE）。两种时序：投递前已中止（①准入丢弃）、
// 复检期间被中止（锁内 realReady 丢弃）。应失败：把重试当成 DISPATCHED 照常下发，或对终态单做 FAILED 迁移。
func TestDispatchReal_RetryOnCanceledOrderDropped(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate       func(f *dispatchFixture)
		wantPrecheck int
	}{
		"canceled before delivery": {mutate: func(f *dispatchFixture) { f.store.order.Status = string(model.OrderCanceled) }},
		"canceled during recheck": {mutate: func(f *dispatchFixture) {
			f.deps.Recheck = cancelingPrecheck{f.pre}
		}, wantPrecheck: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderDispatched)
			f.dispatched()
			f.seedAttempt(1, model.AttemptFailed, model.FailureNotExecuted, "job-0")
			tc.mutate(f)
			if err := f.svc.SetDispatch(f.deps); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Dispatch(context.Background(), realMsg(2)); err != nil {
				t.Fatalf("err = %v, want nil (message dropped)", err)
			}
			if f.real.dispatches+f.real.archives != 0 || f.pre.calls != tc.wantPrecheck {
				t.Fatalf("dispatches=%d archives=%d prechecks=%d", f.real.dispatches, f.real.archives, f.pre.calls)
			}
			if f.store.order.Status != string(model.OrderCanceled) || len(f.store.transitions) != 0 || len(f.store.attempts) != 1 {
				t.Fatalf("status=%s transitions=%+v attempts=%d", f.store.order.Status, f.store.transitions, len(f.store.attempts))
			}
		})
	}
}

// REAL 单而复检未配置：失败关闭（ErrDispatchState），不选设备、不调设备（应失败：跳过复检照常下发）。
func TestDispatchReal_NoRecheckFailsClosed(t *testing.T) {
	f := newRealFixture(t, model.OrderApproved)
	f.deps.Recheck = nil
	if err := f.svc.SetDispatch(f.deps); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Dispatch(context.Background(), realMsg(1)); !errors.Is(err, ErrDispatchState) {
		t.Fatalf("err = %v, want ErrDispatchState", err)
	}
	if f.real.dispatches+f.real.archives+f.picker.calls != 0 || f.store.order.Status != string(model.OrderApproved) {
		t.Fatalf("dispatches=%d picks=%d status=%s", f.real.dispatches, f.picker.calls, f.store.order.Status)
	}
}

// whenDispatching 在设备调用进行中（dispatches 达 1）时执行 fn。
func (f *dispatchFixture) whenDispatching(fn func()) {
	go func() {
		for {
			f.real.mu.Lock()
			n := f.real.dispatches
			f.real.mu.Unlock()
			if n > 0 {
				fn()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// 设备调用进行中锁续期失败 / outbox 租约丢失（红线）：ctx 被取消、设备 IO 中断、返回错误，
// 尝试保持 SENT 无 job、不写 RESULT（应失败：续期失败后照常回填结果）。下次投递走续跑。
func TestDispatchReal_LockOrLeaseLostStops(t *testing.T) {
	for name, want := range map[string]error{"lock renew fails": ErrOrderLockLost, "outbox lease lost": ErrOutboxLeaseLost} {
		t.Run(name, func(t *testing.T) {
			withFastRenew(t)
			f := newRealFixture(t, model.OrderApproved)
			f.store.order.Payload = json.RawMessage(`{"product":"P1","area_mu":12,"device_id":"uav-1"}`)
			lease := &scriptedLease{}
			f.deps.Locks = &leaseLocks{lease: lease}
			if err := f.svc.SetDispatch(f.deps); err != nil {
				t.Fatal(err)
			}
			f.real.block = true
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			f.whenDispatching(func() {
				if want == ErrOutboxLeaseLost {
					cancel(ErrOutboxLeaseLost)
					return
				}
				lease.mu.Lock()
				lease.renewErr = repository.ErrLockLost
				lease.mu.Unlock()
			})
			err := f.svc.Dispatch(ctx, realMsg(1))
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			if len(f.store.attempts) != 1 || f.store.attempts[0].Status != model.AttemptSent ||
				f.store.attempts[0].DeviceJobID != "" {
				t.Fatalf("attempts = %+v", f.store.attempts)
			}
			if got := f.realPhases(t); got != "[INTENT:false]" {
				t.Errorf("DISPATCHED = %s", got)
			}
			lease.mu.Lock()
			defer lease.mu.Unlock()
			if !lease.released {
				t.Error("lock not released")
			}
		})
	}
}
