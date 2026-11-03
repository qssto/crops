package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/model"
)

// T0 后 resume 不调 Adapter 且转人工（L8-B 审查 P0-2）：T0 前写下 SENT、设备调用瞬时失败，T0 后 outbox 重投走续跑。
// 开关关 → 不重发（应失败：旧实现按原 idem_key 直接 DispatchArchived），尝试 FAILED(UNKNOWN) + DISPATCH_MANUAL
// （原因 manualResumeFlagOff），单保持 DISPATCHED；QueryStatus 只读，可在开关检查前；读开关失败 → 返回错误、尝试保持 SENT。
func TestDispatchReal_ResumeFlagOff(t *testing.T) {
	sentBeforeT0 := dispatchNow.Add(-10 * time.Minute)
	for name, tc := range map[string]struct {
		caps     adapter.Capabilities
		queryErr error
		flagErr  error
		wantErr  bool
		queries  int
		status   model.AttemptStatus
	}{
		"idem key, flag off → manual": {caps: adapter.Capabilities{IdemKey: true}, status: model.AttemptFailed},
		"idem+query miss, flag off → manual": {caps: adapter.Capabilities{IdemKey: true, ExternalQuery: true},
			queryErr: adapter.ErrJobNotFound, queries: 1, status: model.AttemptFailed},
		"flag read error keeps SENT": {caps: adapter.Capabilities{IdemKey: true}, flagErr: errors.New("mysql down"),
			wantErr: true, status: model.AttemptSent},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRealFixture(t, model.OrderDispatched)
			f.dispatched()
			f.seedAttempt(1, model.AttemptSent, "", "")
			f.store.attempts[0].FlagReadAt, f.store.attempts[0].SentAt = sentBeforeT0, sentBeforeT0
			f.flags.robot, f.flags.err = false, tc.flagErr
			f.real.caps, f.real.queryErr = tc.caps, tc.queryErr
			err := f.svc.Dispatch(context.Background(), realMsg(1))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %t", err, tc.wantErr)
			}
			if f.real.dispatches != 0 || f.real.queries != tc.queries || f.flags.calls != 1 {
				t.Fatalf("dispatches=%d queries=%d flag reads=%d", f.real.dispatches, f.real.queries, f.flags.calls)
			}
			a := f.store.attempts[0]
			if a.Status != tc.status || a.DeviceJobID != "" || !a.SentAt.Equal(sentBeforeT0) {
				t.Errorf("attempt = %+v", a)
			}
			if f.store.order.Status != string(model.OrderDispatched) || len(f.store.transitions) != 0 {
				t.Errorf("status=%s transitions=%+v", f.store.order.Status, f.store.transitions)
			}
			if tc.wantErr {
				if len(f.audit.recs) != 0 {
					t.Errorf("audit = %+v", f.audit.recs)
				}
				return
			}
			if a.FailureKind != model.FailureUnknown || len(f.audit.recs) != 1 ||
				f.audit.recs[0].Action != string(model.AuditDispatchManual) {
				t.Fatalf("attempt kind=%s audit=%+v", a.FailureKind, f.audit.recs)
			}
			var sum dispatchManual
			if err := json.Unmarshal(f.audit.recs[0].Summary, &sum); err != nil {
				t.Fatal(err)
			}
			if sum.Reason != manualResumeFlagOff || sum.IdemKey != DispatchIdemKey(3, 7, 1) {
				t.Errorf("DISPATCH_MANUAL summary = %+v", sum)
			}
		})
	}
}

// 开关开时续跑重发前刷新 flag_read_at / sent_at / receipt_due_at（关停报告据此覆盖续跑重发）：设备再次超时，尝试保持 SENT，
// 三列都是本次重发的时刻（应失败：保留 T0 前的旧值，报告查不到 T0 后的重发）。
func TestDispatchReal_ResumeRefreshesSent(t *testing.T) {
	old := dispatchNow.Add(-10 * time.Minute)
	f := newRealFixture(t, model.OrderDispatched)
	f.dispatched()
	f.seedAttempt(1, model.AttemptSent, "", "")
	f.store.attempts[0].FlagReadAt, f.store.attempts[0].SentAt = old, old
	f.store.attempts[0].ReceiptDueAt = old.Add(30 * time.Minute)
	f.real.err = adapter.ErrUnavailable
	if err := f.svc.Dispatch(context.Background(), realMsg(1)); err == nil {
		t.Fatal("device timeout must return error")
	}
	a := f.store.attempts[0]
	if f.real.dispatches != 1 || a.Status != model.AttemptSent || a.RowVer != 2 || !a.FlagReadAt.Equal(dispatchNow) ||
		!a.SentAt.Equal(dispatchNow) || !a.ReceiptDueAt.Equal(dispatchNow.Add(30*time.Minute)) {
		t.Fatalf("dispatches=%d attempt=%+v", f.real.dispatches, a)
	}
	if a.FlagReadAt.After(a.SentAt) {
		t.Errorf("flag_read_at %s after sent_at %s", a.FlagReadAt, a.SentAt)
	}
}

// 刷新写失败：不调 Adapter，返回错误交 outbox 重试。
func TestDispatchReal_ResumeRefreshConflict(t *testing.T) {
	f := newRealFixture(t, model.OrderDispatched)
	f.dispatched()
	f.seedAttempt(1, model.AttemptSent, "", "")
	f.store.failOn = "refresh"
	err := f.svc.Dispatch(context.Background(), realMsg(1))
	if err == nil || f.real.dispatches != 0 || f.store.attempts[0].Status != model.AttemptSent {
		t.Fatalf("err=%v dispatches=%d", err, f.real.dispatches)
	}
}
