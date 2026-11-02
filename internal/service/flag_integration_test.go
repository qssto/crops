//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

var flagAdminIT = model.Scope{UserID: 9001, Roles: []model.Role{model.RolePlatformAdmin}}

// itRoles 是 FarmRoleChecker 桩：示范农场有 agronomist。
type itRoles struct{}

func (itRoles) FarmHasRole(_ context.Context, t, f uint64, role model.Role) (bool, error) {
	return t == itest.TenantID && f == itest.FarmID && role == model.RoleAgronomist, nil
}

// failAfterAudit 先真实写审计再返回错误：验证 feature_flag 与 audit_log 同事务回滚。
type failAfterAudit struct{ repository.AuditRepo }

func (a failAfterAudit) AppendTx(ctx context.Context, tx *repository.DB, r model.AuditRecord) error {
	if err := a.AuditRepo.AppendTx(ctx, tx, r); err != nil {
		return err
	}
	return errors.New("injected audit failure")
}

// newFlags 装配真实 MySQL / Redis 上的 FlagService（与 app.buildFlags 同构）；audit 为空取 AuditRepo。
func newFlags(t *testing.T, e *itest.Env, slot *env.Slot, audit service.AuditTxAppender) *service.FlagService {
	t.Helper()
	logger, err := app.NewLogger(os.Stderr, "warn")
	if err != nil {
		t.Fatal(err)
	}
	if audit == nil {
		audit = repository.AuditRepo{DB: slot.DB}
	}
	svc, err := service.NewFlagService(service.FlagDeps{Store: repository.FlagRepo{DB: slot.DB},
		Tx: repository.Transactor{DB: slot.DB}, Audit: audit, Pub: repository.RedisPublisher{Client: e.Redis},
		Sub: repository.RedisSubscriber{Client: e.Redis}, FarmRoles: itRoles{}, Keys: slot.Keys, Clock: slot.Clock,
		Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// P1 §6.1：种子行生效；upsert / 乐观锁 / FLAG_CHANGE 审计同事务（审计失败整体回滚）；三层合并走真实 SQL。
func TestFlag_Integration(t *testing.T) {
	e := itest.Setup(t, "flag")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	svc := newFlags(t, e, slot, nil)
	farm := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	set := func(tn, fm uint64, v string, expect *int) (service.FlagSetResult, error) {
		return svc.Set(ctx, flagAdminIT, service.FlagSetInput{Key: model.FlagEnableAdvice, TenantID: tn, FarmID: fm,
			Value: json.RawMessage(v), Comment: "集成", ExpectRowVer: expect})
	}
	live := func(step string, want bool) {
		t.Helper()
		if on, err := svc.BoolLive(ctx, model.FlagEnableAdvice, farm); err != nil || on != want {
			t.Fatalf("%s: BoolLive = %t, %v; want %t", step, on, err, want)
		}
	}
	live("seed FARM on", true)
	if on, err := svc.BoolLive(ctx, model.FlagEnableRobotDispatch, farm); err != nil || on {
		t.Fatalf("robot default = %t, %v", on, err)
	}
	// upsert：GLOBAL 首次写 row_ver 0，再写 +1；force_off 压过 FARM on，清除后恢复。
	if res, err := set(0, 0, `{"force_off":true}`, nil); err != nil || res.RowVer != 0 || res.ScopeType != model.FlagScopeGlobal {
		t.Fatalf("global insert = %+v, %v", res, err)
	}
	live("global force_off", false)
	if res, err := set(0, 0, `{"force_off":false}`, nil); err != nil || res.RowVer != 1 {
		t.Fatalf("global upsert = %+v, %v", res, err)
	}
	live("global cleared", true)
	// 乐观锁：种子 FARM 行 row_ver 0。
	zero, stale := 0, 0
	if res, err := set(itest.TenantID, itest.FarmID, `{"on":false}`, &zero); err != nil || res.RowVer != 1 {
		t.Fatalf("optimistic update = %+v, %v", res, err)
	}
	if _, err := set(itest.TenantID, itest.FarmID, `{"on":true}`, &stale); !errors.Is(err, service.ErrFlagConflict) {
		t.Fatalf("stale row_ver err = %v", err)
	}
	live("farm off", false)
	checks := []struct {
		what string
		q    string
		want int64
	}{
		{"global row", `SELECT COUNT(*) FROM feature_flag WHERE tenant_id = 0 AND farm_id = 0 AND flag_key = 'enable_advice'
  AND scope_type = 'GLOBAL' AND row_ver = 1 AND JSON_EXTRACT(value, '$.force_off') = false AND updated_by = 9001`, 1},
		{"farm row", `SELECT COUNT(*) FROM feature_flag WHERE tenant_id = 1 AND farm_id = 1 AND flag_key = 'enable_advice'
  AND row_ver = 1 AND JSON_EXTRACT(value, '$.on') = false`, 1},
		{"FLAG_CHANGE audits", `SELECT COUNT(*) FROM audit_log WHERE action = 'FLAG_CHANGE' AND actor_type = 'user'
  AND actor_id = 9001 AND target = 'flag:enable_advice' AND result = 'OK'`, 3},
		{"old/new in summary", `SELECT COUNT(*) FROM audit_log WHERE action = 'FLAG_CHANGE' AND tenant_id = 1 AND farm_id = 1
  AND JSON_EXTRACT(summary, '$.old.on') = true AND JSON_EXTRACT(summary, '$.new.on') = false`, 1},
	}
	for _, c := range checks {
		if got := itest.Count(t, slot.DB, c.q); got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, got, c.want)
		}
	}
	// 审计失败：feature_flag 与 audit_log 都不落。
	bad := newFlags(t, e, slot, failAfterAudit{repository.AuditRepo{DB: slot.DB}})
	if _, err := bad.Set(ctx, flagAdminIT, service.FlagSetInput{Key: model.FlagEnableAdvice, TenantID: itest.TenantID,
		Value: json.RawMessage(`{"force_off":true}`), Comment: "回滚"}); err == nil {
		t.Fatal("want audit failure")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM feature_flag WHERE tenant_id = 1 AND farm_id = 0`); n != 0 {
		t.Errorf("tenant row after rollback = %d", n)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE action = 'FLAG_CHANGE'`); n != 3 {
		t.Errorf("audits after rollback = %d, want 3", n)
	}
	rows, err := svc.List(ctx, flagAdminIT, model.FlagEnableAdvice)
	if err != nil || len(rows) != 2 {
		t.Errorf("List = %+v, %v", rows, err)
	}
}

// 下发器 ②：REAL 单 + enable_robot_dispatch 关 → 0 次 Adapter 调用、APPROVED 不变、outbox DONE、DISPATCH_HELD 审计；
// 开关开（P1-L2 前无真实 Adapter）→ 仍 0 次调用、outbox 留 PENDING 退避重试。
func TestDispatchHeld_Integration(t *testing.T) {
	e := itest.Setup(t, "held")
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	dev := &env.RecordingAdapter{}
	poller, _ := newDispatchPoller(t, e, slot, dev)
	enqueue := func() (uint64, uint64, uint64) {
		runID := itest.InsertRun(t, slot.DB, slot.Clock.Now())
		orderID := itest.InsertOrder(t, slot.DB, runID, "APPROVED", slot.Clock.Now())
		if err := slot.DB.WithContext(ctx).Exec(`UPDATE work_order SET dispatch_mode = 'REAL', dispatch_deadline_at = ?
  WHERE id = ?`, slot.Clock.Now().Add(time.Hour), orderID).Error; err != nil {
			t.Fatal(err)
		}
		return runID, orderID, insertDispatchOutbox(t, slot.DB, orderID, 1, runID, slot.Clock.Now())
	}

	runID, orderID, outboxID := enqueue()
	if n, err := poller.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	checks := []struct {
		what string
		q    string
		args []any
		want int64
	}{
		{"order stays APPROVED", `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'APPROVED' AND dispatch_result IS NULL`,
			[]any{orderID}, 1},
		{"outbox DONE", `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'DONE'`, []any{outboxID}, 1},
		{"DISPATCH_HELD audit", `SELECT COUNT(*) FROM audit_log WHERE run_id = ? AND action = 'DISPATCH_HELD' AND result = 'DENIED'
  AND actor_type = 'system' AND target = CONCAT('order:', ?) AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.dispatch_mode')) = 'REAL'`,
			[]any{runID, orderID}, 1},
		{"no DISPATCHED event", `SELECT COUNT(*) FROM run_event WHERE run_id = ? AND event_type = 'DISPATCHED'`, []any{runID}, 0},
		{"no transition", `SELECT COUNT(*) FROM work_order_transition WHERE order_id = ?`, []any{orderID}, 0},
	}
	for _, c := range checks {
		if got := itest.Count(t, slot.DB, c.q, c.args...); got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, got, c.want)
		}
	}
	if calls := dev.DeviceCalls(); len(calls) != 0 {
		t.Fatalf("adapter calls = %+v, want 0", calls)
	}

	// 开关置开（同农场 enable_advice 已开 + agronomist）：REAL 单按「不可下发」处理。
	flags := newFlags(t, e, slot, nil)
	if _, err := flags.Set(ctx, flagAdminIT, service.FlagSetInput{Key: model.FlagEnableRobotDispatch, TenantID: itest.TenantID,
		FarmID: itest.FarmID, Value: json.RawMessage(`{"on":true}`), Comment: "演练"}); err != nil {
		t.Fatal(err)
	}
	_, orderID2, outboxID2 := enqueue()
	if n, err := poller.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("RunOnce (flag on) = %d, %v", n, err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'PENDING' AND attempts = 1`,
		outboxID2); n != 1 {
		t.Error("flag on: outbox not left PENDING for retry")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'APPROVED'`, orderID2); n != 1 {
		t.Error("flag on: order not APPROVED")
	}
	if calls := dev.DeviceCalls(); len(calls) != 0 {
		t.Fatalf("adapter calls (flag on) = %+v, want 0", calls)
	}
}
