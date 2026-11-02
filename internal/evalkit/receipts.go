package evalkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/app"
	"agri-steward/internal/evalkit/env"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1-L4-B 回执投递步骤（简报接缝 6）：Runner 在审批脚本与 L4-A 的「同步排空 outbox 一轮」之后，
// 按样本 fixtures.device.steps 推进 FrozenClock、经签名 webhook 投递回执、触发回执超时扫描，再记录 Trial.WorkDone。
// 本文件不改 Runner：接入点由调度方合并 L4-A 排空步骤时统一（Drain 即该步骤）。

// workDoneLookback 是记录 LocalVisible 时 LocalOrderRepo 的回看窗口（覆盖样本内时钟推进）。
const workDoneLookback = 30 * 24 * time.Hour

// ReceiptSteps 执行回执投递步骤并记录完成态。Secret 须与 App 装配时的 Deps.ReceiptSecret 相同（评测进程内生成，
// 不落盘）；Drain 为 L4-A 的排空 outbox 步骤（每步之后调用，让重试下发 / 中止 / field.history 回写落地），可为 nil。
type ReceiptSteps struct {
	Secret []byte
	Drain  func(ctx context.Context, a *app.App) error
}

// Run 按序执行样本的回执步骤并把完成态写入 t.WorkDone；样本未声明步骤时不动。App.Clock 须为 *env.FrozenClock。
func (r ReceiptSteps) Run(ctx context.Context, t *Trial, a *app.App, s *sample.Sample, scope model.Scope, runID uint64) error {
	if s.Fixtures.Device == nil || len(s.Fixtures.Device.Steps) == 0 {
		return nil
	}
	if a.DB == nil || a.OrderOps == nil {
		return errors.New("evalkit: receipt steps need App.DB and App.OrderOps")
	}
	clk, ok := a.Clock.(*env.FrozenClock)
	if !ok {
		return errors.New("evalkit: receipt steps need env.FrozenClock")
	}
	refs, err := runOrders(ctx, a, scope, runID)
	if err != nil {
		return err
	}
	var last *webhookCall
	for i, st := range s.Fixtures.Device.Steps {
		if err := r.step(ctx, a, clk, scope, refs, st, &last); err != nil {
			return fmt.Errorf("evalkit: device step %d: %w", i, err)
		}
	}
	return recordWorkDone(ctx, t, a, refs)
}

// step 执行一步：推进时钟 → 投递 / 重放回执或中止 → 扫描 → 排空 outbox。
func (r ReceiptSteps) step(ctx context.Context, a *app.App, clk *env.FrozenClock, scope model.Scope, refs []model.OrderRef,
	st sample.ReceiptStep, last **webhookCall) error {
	d, err := st.AdvanceDuration()
	if err != nil {
		return err
	}
	clk.Advance(d)
	switch {
	case st.Cancel:
		ref, err := nthOrder(refs, st.Order)
		if err != nil {
			return err
		}
		if _, err := a.OrderOps.Cancel(ctx, scope, ref.OrderID); err != nil {
			return fmt.Errorf("cancel order %d: %w", ref.OrderID, err)
		}
	case st.Repeat:
		if *last == nil {
			return errors.New("repeat without a previous receipt")
		}
		if err := r.send(ctx, a, clk, **last); err != nil {
			return fmt.Errorf("repeat: %w", err)
		}
	case st.Receipt != nil:
		call, err := receiptCall(ctx, a, clk, refs, st)
		if err != nil {
			return err
		}
		if err := r.send(ctx, a, clk, call); err != nil {
			return err
		}
		*last = &call
	}
	if st.Scan {
		if err := a.OrderOps.ScanReceiptDueOnce(ctx); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
	}
	if r.Drain == nil {
		return nil
	}
	if err := r.Drain(ctx, a); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return nil
}

// webhookCall 是一次回执投递（重放时原样重投 Body，按当时时钟重新签名）。
type webhookCall struct {
	Ref  model.OrderRef
	Body []byte
}

// receiptBody 与 service 的回执 webhook 请求体同字段。
type receiptBody struct {
	ReceiptID   string    `json:"receipt_id"`
	DeviceType  string    `json:"device_type"`
	DeviceJobID string    `json:"device_job_id"`
	Status      string    `json:"status"`
	FailureKind string    `json:"failure_kind,omitempty"`
	DoneAreaMu  *float64  `json:"done_area_mu,omitempty"`
	DeviceTS    time.Time `json:"device_ts"`
	Code        string    `json:"code,omitempty"`
}

// receiptCall 构造第 st.Order 张作业单（默认 1）最近一次尝试的回执；任务号与设备类型取自该尝试，device_ts 取当前时钟。
func receiptCall(ctx context.Context, a *app.App, clk *env.FrozenClock, refs []model.OrderRef, st sample.ReceiptStep) (webhookCall, error) {
	ref, err := nthOrder(refs, st.Order)
	if err != nil {
		return webhookCall{}, err
	}
	atts, err := repository.DispatchAttemptRepo{DB: a.DB}.ByOrder(ctx, ref)
	if err != nil {
		return webhookCall{}, err
	}
	if len(atts) == 0 || atts[len(atts)-1].DeviceJobID == "" {
		return webhookCall{}, fmt.Errorf("order %d has no accepted dispatch attempt", ref.OrderID)
	}
	at := atts[len(atts)-1]
	rc := st.Receipt
	body, err := json.Marshal(receiptBody{DeviceType: at.DeviceType, DeviceJobID: at.DeviceJobID, Status: rc.Status,
		FailureKind: rc.FailureKind, DoneAreaMu: rc.DoneAreaMu, DeviceTS: clk.Now().UTC(), Code: rc.Code})
	if err != nil {
		return webhookCall{}, fmt.Errorf("encode receipt: %w", err)
	}
	return webhookCall{Ref: ref, Body: body}, nil
}

// send 按当前时钟签名并投递到回执 webhook（与设备平台推送同一入口：验签 → 反查 → Process）。
func (r ReceiptSteps) send(ctx context.Context, a *app.App, clk *env.FrozenClock, c webhookCall) error {
	ts := strconv.FormatInt(clk.Now().Unix(), 10)
	_, err := a.OrderOps.Webhook(ctx, service.WebhookInput{FarmID: c.Ref.FarmID, OrderID: c.Ref.OrderID, Timestamp: ts,
		Signature: service.SignReceipt(r.Secret, ts, c.Body), Body: c.Body, Route: "evalkit.receipt", RequestID: "evalkit-receipt"})
	if err != nil {
		return fmt.Errorf("webhook order %d: %w", c.Ref.OrderID, err)
	}
	return nil
}

// nthOrder 取第 n 张作业单（n 为 0 时取第 1 张）。
func nthOrder(refs []model.OrderRef, n int) (model.OrderRef, error) {
	n = max(n, 1)
	if n > len(refs) {
		return model.OrderRef{}, fmt.Errorf("order %d: run has %d work orders", n, len(refs))
	}
	return refs[n-1], nil
}

// runOrders 返回该 run 的作业单（按首次出现在迁移记录中的次序，与 RecordingAdapter 脚本的 order 一致）。
func runOrders(ctx context.Context, a *app.App, scope model.Scope, runID uint64) ([]model.OrderRef, error) {
	if runID == 0 {
		return nil, errors.New("evalkit: receipt steps need a run")
	}
	ts, err := repository.WorkOrderRepo{}.TransitionsByRun(ctx, a.DB, scope, runID)
	if err != nil {
		return nil, fmt.Errorf("evalkit: run %d transitions: %w", runID, err)
	}
	var refs []model.OrderRef
	for _, tr := range ts {
		if !slices.Contains(refs, tr.Ref) {
			refs = append(refs, tr.Ref)
		}
	}
	return refs, nil
}

// recordWorkDone 读各作业单完成态：work_order 状态与 done_area_mu、LocalOrderRepo 是否读到（PHI 推算来源）、
// 是否已回写 field.history（只认 mock 的 *service.MemFieldHistory；其他写端记为未回写）。
func recordWorkDone(ctx context.Context, t *Trial, a *app.App, refs []model.OrderRef) error {
	mem, _ := a.FieldHistory.(*service.MemFieldHistory) // 非 mock 写端无记录可读：FieldHistory 一律 false
	since := a.Clock.Now().Add(-workDoneLookback)
	t.WorkDone = make([]grader.WorkDoneObs, 0, len(refs))
	for _, ref := range refs {
		row, err := repository.DeviceReceiptRepo{DB: a.DB}.WorkDone(ctx, ref)
		if err != nil {
			return fmt.Errorf("evalkit: record work done: %w", err)
		}
		scope := model.Scope{TenantID: ref.TenantID, FarmID: ref.FarmID}
		local, err := repository.LocalOrderRepo{DB: a.DB}.RecentOrders(ctx, scope, row.FieldID, 0, since)
		if err != nil {
			return fmt.Errorf("evalkit: record work done: %w", err)
		}
		obs := grader.WorkDoneObs{OrderID: ref.OrderID, Status: row.Status, DoneAreaMu: row.DoneAreaMu}
		obs.LocalVisible = slices.ContainsFunc(local, func(o agent.LocalOrder) bool { return o.OrderID == ref.OrderID })
		if mem != nil {
			obs.FieldHistory = slices.ContainsFunc(mem.Records(ref.TenantID, ref.FarmID, row.FieldID),
				func(h service.FieldHistoryRecord) bool { return h.OrderID == ref.OrderID })
		}
		t.WorkDone = append(t.WorkDone, obs)
	}
	return nil
}
