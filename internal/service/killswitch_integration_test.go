//go:build integration

package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agri-steward/internal/adapter"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/itest"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// noRedteam 是 RedteamReader 桩：本测试只核对关停演练项，评测库不接入。
type noRedteam struct{}

func (noRedteam) LatestExperimentWithSuite(context.Context, string) (repository.EvalExperiment, bool, error) {
	return repository.EvalExperiment{}, false, nil
}

func (noRedteam) SuiteTrials(context.Context, uint64, string) ([]repository.EvalTrial, error) {
	return nil, nil
}

// ksDeviceType 是测试作业单的 device_type（itest.InsertOrder 写 DRONE）。
const ksDeviceType = "DRONE"

// ksDevice 是登记在下发器设备注册表里的真实通道桩（Traced 的 DeviceAdapter + CommandArchiver + ArchivedDispatcher 面），
// 平台按 idem_key 去重（caps.IdemKey），记录调用次数；failNext 让下一次 DispatchArchived 超时（尝试保持 SENT、无 device_job_id）。
type ksDevice struct {
	mu         sync.Mutex
	archives   int
	dispatches int
	failNext   bool
}

func (d *ksDevice) failNextDispatch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNext = true
}

func (*ksDevice) Capabilities() adapter.Capabilities { return adapter.Capabilities{IdemKey: true} }

func (*ksDevice) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, errors.New("untraced Dispatch must not be called")
}

func (*ksDevice) QueryStatus(context.Context, adapter.StatusRef) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, adapter.ErrJobNotFound
}

func (*ksDevice) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, adapter.ErrCapability
}

func (d *ksDevice) Archive(context.Context, model.WorkOrder, string) (model.RawObject, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.archives++
	return model.RawObject{ID: 1}, nil
}

func (d *ksDevice) DispatchArchived(_ context.Context, _ model.WorkOrder, idemKey string, _ uint64) (model.DeviceReceipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dispatches++
	if d.failNext {
		d.failNext = false
		return model.DeviceReceipt{}, errors.New("it device timeout")
	}
	return model.DeviceReceipt{Status: model.AttemptAccepted, DeviceJobID: "it-job-" + idemKey}, nil
}

func (d *ksDevice) calls() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.archives, d.dispatches
}

type ksRegistry map[string]adapter.DeviceAdapter

func (r ksRegistry) Get(t string) (adapter.DeviceAdapter, error) {
	if a, ok := r[t]; ok {
		return a, nil
	}
	return nil, adapter.ErrDeviceNotOpen
}

// ksRecheck 是 service 层复检桩：一律放行；holdNext 后下一次调用先发 entered 再阻塞到 gate 关闭
// （下发器此时已读过开关、尚未写 SENT，用来让在途单跨过 T0）。
type ksRecheck struct {
	mu      sync.Mutex
	hold    bool
	entered chan struct{}
	gate    chan struct{}
}

func newKSRecheck() *ksRecheck {
	return &ksRecheck{entered: make(chan struct{}, 1), gate: make(chan struct{})}
}

func (r *ksRecheck) holdNext() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hold = true
}

func (r *ksRecheck) PreDispatch(ctx context.Context, _ service.PreDispatchInput) (service.PreDispatchResult, error) {
	r.mu.Lock()
	hold := r.hold
	r.hold = false
	r.mu.Unlock()
	if hold {
		r.entered <- struct{}{}
		select {
		case <-r.gate:
		case <-ctx.Done():
			return service.PreDispatchResult{}, ctx.Err()
		}
	}
	return service.PreDispatchResult{}, nil
}

// ksStore 是生产 DispatchRepo，只把复检原问题换成桩（测试 run 没有 RUN_STARTED 事件）。
type ksStore struct{ repository.DispatchRepo }

func (ksStore) RecheckSource(context.Context, model.Scope, uint64) (repository.RecheckSource, error) {
	return repository.RecheckSource{UserID: 1001, FieldID: 1}, nil
}

type ksCards struct{}

func (ksCards) GetByRun(context.Context, model.Scope, uint64) (model.AdviceCard, error) {
	return model.AdviceCard{}, nil
}

type ksPicker struct{}

func (ksPicker) PickDevice(context.Context, model.WorkOrder, int) (string, error) {
	return "uav-it-1", nil
}

type ksReopener struct{}

func (ksReopener) Prepare(context.Context, service.ReopenRequest) (service.ReopenPlan, error) {
	return service.ReopenPlan{}, errors.New("reopen not expected")
}

func (ksReopener) Reopen(context.Context, service.ReopenPlan) (model.ApprovalTicket, error) {
	return model.ApprovalTicket{}, errors.New("reopen not expected")
}

// ksDispatcher 是真实下发器（app.Build 装配的 WorkOrderService.Dispatch）：重新 SetDispatch，设备注册表、选设备、复检
// 换成桩，其余（dispatch_attempt、幂等键、开关直读、order 锁、审批票、审计）全是生产仓储。返回 poller 处理 HELD 单。
func ksDispatcher(t *testing.T, e *itest.Env, slot *env.Slot, dev *ksDevice, pre *ksRecheck) (*service.OutboxPoller,
	*service.WorkOrderService) {
	t.Helper()
	a := newDispatchApp(t, e, slot, &env.RecordingAdapter{})
	cfg := a.DispatchConfig
	d := service.DispatchDeps{Adapter: a.Adapter, Store: ksStore{repository.NewDispatchRepo(slot.DB)},
		Tx: repository.NewDecisionRepo(slot.DB, slot.Clock), Cards: ksCards{}, Epochs: a.Runs, Locks: a.Locker, Keys: a.Keys,
		Logger: a.Logger, Flags: a.Flags, Audit: repository.AuditRepo{DB: slot.DB}, Devices: ksRegistry{ksDeviceType: dev},
		Attempts: repository.DispatchAttemptRepo{DB: slot.DB}, Idem: repository.NewRedisIdem(e.Redis), Picker: ksPicker{},
		Recheck: pre, Reopener: ksReopener{}, MaxAttempts: cfg.MaxAttempts, ReceiptTimeout: cfg.ReceiptTimeout,
		DeviceWaitMax: cfg.DeviceWaitMax, Notify: repository.NotifyOutboxRepo{}, OfflineTextKey: "device_offline",
		Tickets: repository.ReopenRepo{DB: slot.DB}}
	if err := a.WorkOrders.SetDispatch(d); err != nil {
		t.Fatal(err)
	}
	p, err := a.NewOutboxPoller()
	if err != nil {
		t.Fatal(err)
	}
	return p, a.WorkOrders
}

// ksOrder 是一张 APPROVED REAL 单与对应的 order.dispatch 消息（attempt=1，dedup_key 按生产格式）。
type ksOrder struct {
	id, ticket, run uint64
	msg             repository.OutboxMessage
}

// ksRealOrder 新建 APPROVED REAL 单 + APPROVED 票（截止 1h 后），不写 outbox（直接交给 Dispatch 的用例用 msg）。
func ksRealOrder(t *testing.T, slot *env.Slot) ksOrder {
	t.Helper()
	now := slot.Clock.Now()
	runID := itest.InsertRun(t, slot.DB, now)
	orderID := itest.InsertOrder(t, slot.DB, runID, "APPROVED", now)
	execSQL(t, slot.DB, `UPDATE work_order SET dispatch_mode = 'REAL', dispatch_deadline_at = ? WHERE id = ?`,
		now.Add(time.Hour), orderID)
	ticketID := itest.InsertTicket(t, slot.DB, runID, orderID, "APPROVED", now)
	payload, err := json.Marshal(map[string]uint64{"order_id": orderID, "ticket_id": ticketID, "run_id": runID})
	if err != nil {
		t.Fatal(err)
	}
	return ksOrder{id: orderID, ticket: ticketID, run: runID, msg: repository.OutboxMessage{TenantID: itest.TenantID,
		FarmID: itest.FarmID, Topic: repository.TopicOrderDispatch, AggregateID: orderID, Payload: payload,
		DedupKey: service.DispatchIdemKey(orderID, ticketID, 1)}}
}

// ksSetRobot 经 L0 流程（FlagService.Set，平台链 FLAG_CHANGE）写 enable_robot_dispatch；tn=fm=0 为 GLOBAL。
func ksSetRobot(ctx context.Context, flags *service.FlagService, tn, fm uint64, v string) error {
	_, err := flags.Set(ctx, flagAdminIT, service.FlagSetInput{Key: model.FlagEnableRobotDispatch, TenantID: tn, FarmID: fm,
		Value: json.RawMessage(v), Comment: "关停演练"})
	return err
}

func ksReporter(t *testing.T, slot *env.Slot) *service.KillSwitchReporter {
	t.Helper()
	k, err := service.NewKillSwitchReporter(service.KillSwitchDeps{Store: repository.KillSwitchRepo{DB: slot.DB},
		Flags: repository.FlagRepo{DB: slot.DB}, Audit: repository.AuditRepo{DB: slot.DB}, Clock: slot.Clock})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// insertRealAttempt 直接写一条 REAL 尝试：只给应失败用例构造「下发器不该写出的」行（T0 后读开关仍发出、超 60s）。
func insertRealAttempt(t *testing.T, slot *env.Slot, flagReadAt, sentAt time.Time) {
	t.Helper()
	o := ksRealOrder(t, slot)
	execSQL(t, slot.DB, `UPDATE work_order SET status = 'DISPATCHED' WHERE id = ?`, o.id)
	execSQL(t, slot.DB, `INSERT INTO dispatch_attempt (tenant_id, farm_id, order_id, ticket_id, attempt, device_type, device_id,
  idem_key, mode, status, command_raw_id, flag_read_at, sent_at, receipt_due_at, updated_at)
VALUES (?, ?, ?, ?, 1, ?, 'uav-it-1', ?, 'REAL', 'SENT', 1, ?, ?, ?, ?)`, itest.TenantID, itest.FarmID, o.id, o.ticket,
		ksDeviceType, o.msg.DedupKey, flagReadAt, sentAt, sentAt.Add(time.Hour), sentAt)
}

// waitDone 等 Dispatch 协程返回（30s 超时即失败）。
func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("dispatch did not return within 30s")
		return nil
	}
}

// TestKillSwitchDrill_Integration（P1 §6.9 验收 3 的工程演练），子测试各取新槽位、互不依赖：
//   - drill：真实下发器上 T0 前读开关的在途单 T0 后发出（报告读到下发器自写的 flag_read_at < T0）、T0 前超时留下的 SENT 尝试
//     T0 后续跑不调设备转人工（P0-2）、预置单全部 HELD；报告通过并写 KILL_SWITCH_DRILL，gray-preflight 演练项 PASS；
//     解除后 HELD 单不补发，解除后的正常下发不计入报告。
//   - 应失败：T0 后读开关仍发出、耗时 > 60s、HELD 少于预置单，各自只报对应一条失败。
func TestKillSwitchDrill_Integration(t *testing.T) {
	e := itest.Setup(t, "killswitch")
	t.Run("drill", func(t *testing.T) { ksDrill(t, e) })
	cases := []struct {
		name      string
		attempt   bool
		read      time.Duration // flag_read_at − T0
		sent      time.Duration // sent_at − T0
		expect    int64
		wantFails string
	}{
		{"REAL read flag after T0", true, time.Second, 3 * time.Second, 0, "read the flag at or after T0"},
		{"elapsed over 60s", true, -time.Second, service.KillSwitchMaxElapsed + time.Second, 0, "61000ms > 60000ms"},
		{"held below preset", false, 0, 0, 1, "DISPATCH_HELD total 0 < unsent preset orders 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slot := e.Fresh(t, t0)
			ctx := t.Context()
			flags := newFlags(t, e, slot, nil)
			if err := ksSetRobot(ctx, flags, itest.TenantID, itest.FarmID, `{"on":true}`); err != nil {
				t.Fatal(err)
			}
			slot.Clock.Advance(time.Second)
			t0Kill := slot.Clock.Now()
			if err := ksSetRobot(ctx, flags, 0, 0, `{"force_off":true}`); err != nil {
				t.Fatal(err)
			}
			if tc.attempt {
				insertRealAttempt(t, slot, t0Kill.Add(tc.read), t0Kill.Add(tc.sent))
			}
			rep, err := ksReporter(t, slot).Report(ctx, t0Kill.Add(-time.Minute), tc.expect)
			if err != nil {
				t.Fatal(err)
			}
			if !rep.T0.Equal(t0Kill) || len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], tc.wantFails) {
				t.Fatalf("report T0 %s failures %q, want T0 %s and exactly one failure containing %q", rep.T0, rep.Failures,
					t0Kill, tc.wantFails)
			}
		})
	}
}

// ksDrill 是演练主流程；A、B、C 与预置单都走真实下发器（Dispatch / poller）。
func ksDrill(t *testing.T, e *itest.Env) {
	slot := e.Fresh(t, t0)
	ctx := t.Context()
	dev, pre := &ksDevice{}, newKSRecheck()
	poller, wo := ksDispatcher(t, e, slot, dev, pre)
	flags := newFlags(t, e, slot, nil)
	farm := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	if err := ksSetRobot(ctx, flags, itest.TenantID, itest.FarmID, `{"on":true}`); err != nil {
		t.Fatal(err)
	}
	// T0 前：A 正常发出（不计入报告）；B 设备超时，尝试保持 SENT 无 device_job_id（留给 T0 后续跑）。
	a := ksRealOrder(t, slot)
	if err := wo.Dispatch(ctx, a.msg); err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	dev.failNextDispatch()
	b := ksRealOrder(t, slot)
	if err := wo.Dispatch(ctx, b.msg); err == nil {
		t.Fatal("dispatch B: device timeout want error (attempt stays SENT)")
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM dispatch_attempt WHERE order_id = ? AND status = 'SENT'
  AND device_job_id IS NULL`, b.id); n != 1 {
		t.Fatalf("order B SENT attempts without job = %d, want 1", n)
	}

	// 在途 C：下发器在 readAt 读到开关为开，复检阻塞跨过 T0，T0 + 2s 才写 SENT 并调设备。
	c := ksRealOrder(t, slot)
	slot.Clock.Advance(time.Second)
	readAt := slot.Clock.Now()
	pre.holdNext()
	done := make(chan error, 1)
	go func() { done <- wo.Dispatch(ctx, c.msg) }()
	select {
	case <-pre.entered:
	case err := <-done:
		t.Fatalf("dispatch C returned before recheck: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("dispatch C never reached recheck")
	}
	// GLOBAL 只接受 force_off：写 on 被拒，不落行不落审计。
	if err := ksSetRobot(ctx, flags, 0, 0, `{"on":true}`); !errors.Is(err, service.ErrFlagValue) {
		t.Fatalf("GLOBAL on err = %v, want ErrFlagValue", err)
	}
	slot.Clock.Advance(time.Second)
	t0Kill := slot.Clock.Now()
	if err := ksSetRobot(ctx, flags, 0, 0, `{"force_off":true}`); err != nil {
		t.Fatal(err)
	}
	slot.Clock.Advance(2 * time.Second)
	close(pre.gate)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("dispatch C: %v", err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM dispatch_attempt WHERE order_id = ? AND mode = 'REAL'
  AND flag_read_at = ? AND sent_at = ? AND device_job_id IS NOT NULL`, c.id, readAt, t0Kill.Add(2*time.Second)); n != 1 {
		t.Fatalf("in-flight C attempt (flag_read_at %s < T0 %s, sent T0+2s, accepted) = %d rows, want 1", readAt, t0Kill, n)
	}
	archives, dispatches := dev.calls()
	if archives != 3 || dispatches != 3 {
		t.Fatalf("device calls before resume = %d archive / %d dispatch, want 3 / 3", archives, dispatches)
	}

	// P0-2：T0 后 B 的同一条消息重投 → resumeLocked 直读开关为关 → 不调设备，FAILED(UNKNOWN) + DISPATCH_MANUAL，单保持 DISPATCHED。
	slot.Clock.Advance(time.Second)
	if err := wo.Dispatch(ctx, b.msg); err != nil {
		t.Fatalf("resume B after T0: %v", err)
	}
	if a2, d2 := dev.calls(); a2 != archives || d2 != dispatches {
		t.Fatalf("resume B after T0 called device: %d archive / %d dispatch, want %d / %d", a2, d2, archives, dispatches)
	}
	resumeChecks := []struct {
		what string
		q    string
		args []any
	}{
		{"B attempt FAILED(UNKNOWN), sent_at untouched", `SELECT COUNT(*) FROM dispatch_attempt WHERE order_id = ?
  AND status = 'FAILED' AND failure_kind = 'UNKNOWN' AND device_job_id IS NULL AND sent_at < ?`, []any{b.id, t0Kill}},
		{"B DISPATCH_MANUAL with flag-off reason", `SELECT COUNT(*) FROM audit_log WHERE tenant_id = ? AND farm_id = ?
  AND action = 'DISPATCH_MANUAL' AND target = ? AND result = 'FAILED' AND JSON_UNQUOTE(JSON_EXTRACT(summary, '$.reason')) = ?`,
			[]any{itest.TenantID, itest.FarmID, fmt.Sprintf("order:%d", b.id),
				"SENT without device_job_id; resend blocked because enable_robot_dispatch is off"}},
		{"B still DISPATCHED", `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'DISPATCHED'`, []any{b.id}},
	}
	for _, chk := range resumeChecks {
		if n := itest.Count(t, slot.DB, chk.q, chk.args...); n != 1 {
			t.Errorf("%s: got %d, want 1", chk.what, n)
		}
	}

	// 预置单经 poller：直读为关 → 全部 DISPATCH_HELD、单保持 APPROVED、outbox DONE、0 次设备调用。
	const preset = 3
	var orders, outboxes []uint64
	for range preset {
		o := ksRealOrder(t, slot)
		orders = append(orders, o.id)
		outboxes = append(outboxes, insertDispatchOutbox(t, slot.DB, o.id, o.ticket, o.run, slot.Clock.Now()))
	}
	slot.Clock.Advance(time.Second)
	for range preset + 1 {
		if n, err := poller.RunOnce(ctx); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	for i, id := range orders {
		if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'APPROVED'`, id); n != 1 {
			t.Errorf("held order %d not APPROVED", id)
		}
		if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE id = ? AND status = 'DONE'`, outboxes[i]); n != 1 {
			t.Errorf("outbox %d not DONE", outboxes[i])
		}
	}
	if a2, d2 := dev.calls(); a2 != archives || d2 != dispatches {
		t.Fatalf("device calls after HELD = %d / %d, want %d / %d", a2, d2, archives, dispatches)
	}
	if on, err := flags.BoolLive(ctx, model.FlagEnableAdvice, farm); err != nil || !on {
		t.Fatalf("enable_advice during kill switch = %t, %v; want true", on, err)
	}

	// 报告：只有 C 计入（A、B 的 sent_at 在 T0 前），C 的 flag_read_at < T0，耗时 2s，HELD = 预置单数，尚未解除。
	k := ksReporter(t, slot)
	since := t0Kill.Add(-time.Minute)
	rep, err := k.Report(ctx, since, preset)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.T0.Equal(t0Kill) || rep.ElapsedMS != 2000 || rep.RealSentAfter != 1 || rep.FlagReadAfter != 0 ||
		rep.HeldTotal != preset || rep.ReleasedAt != nil || !rep.Passed() {
		t.Fatalf("report = %+v", rep)
	}
	if err := k.RecordDrill(ctx, rep); err != nil {
		t.Fatal(err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM audit_log WHERE tenant_id = 0 AND farm_id = 0
  AND action = 'KILL_SWITCH_DRILL' AND result = 'OK' AND chain_seq IS NOT NULL AND JSON_EXTRACT(summary, '$.elapsed_ms') = 2000
  AND JSON_EXTRACT(summary, '$.flag_change_audit_id') = ?`, rep.FlagChangeID); n != 1 {
		t.Fatalf("KILL_SWITCH_DRILL audits = %d, want 1", n)
	}
	g, err := service.NewGrayPreflight(service.GrayPreflightDeps{Files: os.DirFS(e.Root), Drills: repository.KillSwitchRepo{DB: slot.DB},
		Redteam: noRedteam{}, FarmRoles: repository.UserRoleRepo{DB: slot.DB}, Flags: repository.FlagRepo{DB: slot.DB},
		Clock: slot.Clock})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := g.Run(ctx, farm)
	if err != nil {
		t.Fatal(err)
	}
	// D5 / 法务签字待签字、redteam 未接评测库：FAIL 是预期；演练项回查到真实 T0 的 FLAG_CHANGE。
	want := map[string]bool{service.CheckD5Closed: false, service.CheckLegalSignoff: false, service.CheckAlertRules: true,
		service.CheckKillSwitchTest: true, service.CheckRedteamPassK: false, service.CheckAgronomist: true, service.CheckAdviceOn: true}
	for _, r := range rs {
		if r.Pass != want[r.Name] {
			t.Errorf("preflight %s pass = %t, want %t (%s)", r.Name, r.Pass, want[r.Name], r.Detail)
		}
		if r.Name == service.CheckKillSwitchTest && !strings.Contains(r.Detail, fmt.Sprintf("(FLAG_CHANGE %d)", rep.FlagChangeID)) {
			t.Errorf("drill detail %q missing FLAG_CHANGE %d", r.Detail, rep.FlagChangeID)
		}
	}
	ksRelease(t, slot, poller, wo, flags, dev, orders, rep)
}

// ksRelease：解除后只有 FARM 行为开的农场恢复；HELD 单不自动补发（outbox 已 DONE，无 DISPATCHED 事件）；
// 解除后的正常下发 D 不计入报告（计数上界 = 解除时刻），报告其余结论不变。
func ksRelease(t *testing.T, slot *env.Slot, poller *service.OutboxPoller, wo *service.WorkOrderService,
	flags *service.FlagService, dev *ksDevice, held []uint64, before service.KillSwitchReport) {
	t.Helper()
	ctx := t.Context()
	slot.Clock.Advance(time.Second)
	releasedAt := slot.Clock.Now()
	if err := ksSetRobot(ctx, flags, 0, 0, `{"force_off":false}`); err != nil {
		t.Fatal(err)
	}
	farm := model.Scope{TenantID: itest.TenantID, FarmID: itest.FarmID}
	if on, err := flags.BoolLive(ctx, model.FlagEnableRobotDispatch, farm); err != nil || !on {
		t.Fatalf("gray farm after release = %t, %v; want true", on, err)
	}
	other := model.Scope{TenantID: itest.TenantID, FarmID: 2}
	if on, err := flags.BoolLive(ctx, model.FlagEnableRobotDispatch, other); err != nil || on {
		t.Fatalf("non-gray farm after release = %t, %v; want false", on, err)
	}
	_, dispatches := dev.calls()
	slot.Clock.Advance(time.Minute)
	if _, err := poller.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce after release: %v", err)
	}
	if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM outbox WHERE topic = ? AND status <> 'DONE'`,
		repository.TopicOrderDispatch); n != 0 {
		t.Errorf("order.dispatch not DONE after release: %d", n)
	}
	if _, d2 := dev.calls(); d2 != dispatches {
		t.Fatalf("device dispatches after release = %d, want %d (HELD orders not re-sent)", d2, dispatches)
	}
	for _, id := range held {
		if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM run_event e JOIN work_order o ON o.run_id = e.run_id
  WHERE o.id = ? AND e.event_type = 'DISPATCHED'`, id); n != 0 {
			t.Errorf("held order %d re-dispatched after release", id)
		}
		if n := itest.Count(t, slot.DB, `SELECT COUNT(*) FROM work_order WHERE id = ? AND status = 'APPROVED'`, id); n != 1 {
			t.Errorf("held order %d not left APPROVED for manual handling", id)
		}
	}
	d := ksRealOrder(t, slot)
	if err := wo.Dispatch(ctx, d.msg); err != nil {
		t.Fatalf("dispatch D after release: %v", err)
	}
	if _, d2 := dev.calls(); d2 != dispatches+1 {
		t.Fatalf("device dispatches after D = %d, want %d", d2, dispatches+1)
	}
	rep, err := ksReporter(t, slot).Report(ctx, before.T0.Add(-time.Minute), before.ExpectHeld)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReleasedAt == nil || !rep.ReleasedAt.Equal(releasedAt) || rep.ReleaseID == 0 || !rep.T0.Equal(before.T0) ||
		rep.RealSentAfter != before.RealSentAfter || rep.ElapsedMS != before.ElapsedMS || rep.HeldTotal != before.HeldTotal ||
		!rep.Passed() {
		t.Fatalf("report after release = %+v, want released at %s and counts unchanged from %+v", rep, releasedAt, before)
	}
}
