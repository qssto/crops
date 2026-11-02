package service

import (
	"context"
	"fmt"
	"strconv"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/repository/rediskey"
)

// ReceiptInput 是一条待处理的标准回执：webhook（反查归属后）、超时扫描的 QueryStatus 结果、中止处理器的 Cancel 结果共用。
type ReceiptInput struct {
	Ref       model.OrderRef
	AttemptID uint64
	ReceiptID string // 平台回执号；平台未给时由 ReceiptIDOf 派生
	Receipt   model.DeviceReceipt
	Source    model.ReceiptSource
	Raw       []byte // 回执原文；非空时先存证 RECEIPT_RAW，device_receipt.raw_object_id 指向它
}

// ReceiptResult 是回执处理结论。Replayed 为 true 表示同一 receipt_id 已处理过（未改任何状态，Outcome 取首次结论，可能为空）。
type ReceiptResult struct {
	Outcome     model.ReceiptOutcome
	Replayed    bool
	OrderStatus model.WorkOrderStatus // 处理后的工单状态（重放时为空）
	alert       string                // 提交后才打的告警（事务回滚不误报）
}

// receiptIDMax 是 device_receipt.receipt_id 的列宽。
const receiptIDMax = 96

// ReceiptIDOf 派生回执号：平台给了就用平台的；否则取 sha256(source|job|status|kind|area|device_ts)，同一回执重投得到同一号。
func ReceiptIDOf(given string, src model.ReceiptSource, r model.DeviceReceipt) string {
	if given != "" {
		return given
	}
	area := ""
	if r.DoneAreaMu != nil {
		area = strconv.FormatFloat(*r.DoneAreaMu, 'f', 2, 64)
	}
	body := fmt.Sprintf("%s|%s|%s|%s|%s|%d", src, r.DeviceJobID, r.Status, r.FailureKind, area, r.DeviceTime.UnixMilli())
	return string(src) + "-" + repository.Digest([]byte(body))[:40]
}

func (in ReceiptInput) validate() error {
	switch {
	case in.Ref.TenantID == 0 || in.Ref.FarmID == 0 || in.Ref.OrderID == 0 || in.AttemptID == 0:
		return fmt.Errorf("%w: receipt needs tenant / farm / order / attempt", ErrBadInput)
	case in.ReceiptID == "" || len(in.ReceiptID) > receiptIDMax:
		return fmt.Errorf("%w: receipt_id length must be 1..%d", ErrBadInput, receiptIDMax)
	case !in.Source.Valid():
		return fmt.Errorf("%w: receipt source %q", ErrBadInput, in.Source)
	}
	if err := in.Receipt.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	return nil
}

// Process 是回执处理函数（L4-A 接缝：webhook / 轮询 / 中止共用，§6.5 技术实现第 6 步）：
// idem:receipt 命中即重放 → 原文存证 → 持 lock:order 同一事务 INSERT IGNORE device_receipt（0 行即重放，不处理）→
// 判 effect（APPLIED / STALE / LATE / CONFLICT）→ APPLIED 时更新 dispatch_attempt、按 model.MapReceipt 迁移或重试 / 转人工，
// REAL 单进入 DONE / PARTIALLY_DONE 同事务回写。事务失败整体回滚返回错误（webhook 5xx）；提交后才写 idem:receipt 7d。
func (s *OrderOpsService) Process(ctx context.Context, in ReceiptInput) (ReceiptResult, error) {
	if err := in.validate(); err != nil {
		return ReceiptResult{}, err
	}
	key := s.d.Keys.IdemReceipt(in.ReceiptID)
	if v, ok, err := s.d.Idem.Get(ctx, key); err != nil {
		s.d.Logger.WarnContext(ctx, "receipt: idem read failed, fall back to uk_receipt", "receipt_id", in.ReceiptID, "err", err)
	} else if ok {
		if o, match := parseIdem(v, in.Ref); match {
			return ReceiptResult{Outcome: o, Replayed: true}, nil
		}
	}
	rawID, err := s.archiveReceipt(ctx, in.Ref, in.ReceiptID, in.Raw)
	if err != nil {
		return ReceiptResult{}, err
	}
	var res ReceiptResult
	if err := WithOrderLock(ctx, s.d.Locks, s.d.Keys, s.d.Logger, in.Ref.OrderID, func(lctx context.Context) error {
		return s.d.Tx.InTx(lctx, func(tx *repository.DB) error {
			r, err := s.applyReceiptTx(lctx, tx, in, rawID)
			res = r
			return err
		})
	}); err != nil {
		return ReceiptResult{}, fmt.Errorf("service: receipt %s order %d: %w", in.ReceiptID, in.Ref.OrderID, err)
	}
	if res.alert != "" {
		s.d.Logger.ErrorContext(ctx, "receipt: needs attention", "alert", res.alert, "order_id", in.Ref.OrderID,
			"attempt_id", in.AttemptID, "receipt_id", in.ReceiptID, "status", string(in.Receipt.Status),
			"failure_kind", string(in.Receipt.FailureKind), "outcome", string(res.Outcome))
	}
	if _, err := s.d.Idem.SetNX(ctx, key, idemValue(in.Ref, res.Outcome), rediskey.IdemReceiptTTL); err != nil {
		s.d.Logger.WarnContext(ctx, "receipt: idem write failed", "receipt_id", in.ReceiptID, "err", err)
	}
	return res, nil
}

// archiveReceipt 把回执原文存证为 RECEIPT_RAW（Name 带回执号摘要，同一回执重投覆盖同一对象）；raw 为空返回 0。
func (s *OrderOpsService) archiveReceipt(ctx context.Context, ref model.OrderRef, receiptID string, raw []byte) (uint64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	now := s.d.Clock.Now()
	obj, err := s.d.Raw.Put(ctx, model.RawPut{TenantID: ref.TenantID, FarmID: ref.FarmID, Kind: model.RawReceiptRaw,
		Name: "receipt-" + strconv.FormatUint(ref.OrderID, 10) + "-" + repository.Digest([]byte(receiptID))[:16] + ".json",
		At:   now, CreatedAt: now, Body: raw, ContentType: "application/json"})
	if err != nil {
		return 0, fmt.Errorf("service: archive receipt order %d: %w", ref.OrderID, err)
	}
	return obj.ID, nil
}

// applyReceiptTx 在事务内入库并处置一条回执。
func (s *OrderOpsService) applyReceiptTx(ctx context.Context, tx *repository.DB, in ReceiptInput, rawID uint64) (ReceiptResult, error) {
	att, err := s.d.Store.AttemptTx(ctx, tx, in.Ref, in.AttemptID)
	if err != nil {
		return ReceiptResult{}, err
	}
	if in.Receipt.DeviceJobID != "" && att.DeviceJobID != "" && in.Receipt.DeviceJobID != att.DeviceJobID {
		return ReceiptResult{}, fmt.Errorf("%w: receipt job %q, attempt %d job %q", ErrBadInput, in.Receipt.DeviceJobID,
			att.ID, att.DeviceJobID)
	}
	o, err := s.d.Store.OrderForDispatch(ctx, tx, in.Ref)
	if err != nil {
		return ReceiptResult{}, err
	}
	cur := model.WorkOrderStatus(o.Status)
	outcome := classifyReceipt(cur, att, in.Receipt)
	now := s.d.Clock.Now()
	deviceTS := in.Receipt.DeviceTime
	if deviceTS.IsZero() {
		deviceTS = now
	}
	inserted, err := s.d.Store.InsertReceiptTx(ctx, tx, repository.ReceiptRow{TenantID: in.Ref.TenantID, FarmID: in.Ref.FarmID,
		ReceiptID: in.ReceiptID, OrderID: in.Ref.OrderID, Attempt: att.Attempt, DeviceJobID: jobOf(in.Receipt, att),
		NormStatus: in.Receipt.Status, Effect: outcome, DoneAreaMu: in.Receipt.DoneAreaMu, DeviceTS: deviceTS.UTC(),
		Source: in.Source, RawObjectID: rawID, CreatedAt: now})
	if err != nil {
		return ReceiptResult{}, err
	}
	if !inserted {
		return ReceiptResult{Replayed: true}, nil
	}
	res := ReceiptResult{Outcome: outcome, OrderStatus: cur}
	switch outcome {
	case model.OutcomeConflict:
		res.alert = alertReceiptConflict
		return res, nil
	case model.OutcomeApplied:
		return s.applyAdvance(ctx, tx, o, att, in.Receipt, now)
	}
	return res, nil
}

// jobOf 取回执的任务号，回执未带（FAILED 可无）时用尝试已记的。
func jobOf(r model.DeviceReceipt, att model.DispatchAttempt) string {
	if r.DeviceJobID != "" {
		return r.DeviceJobID
	}
	if att.DeviceJobID != "" {
		return att.DeviceJobID
	}
	return "-"
}
