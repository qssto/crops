package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// ReceiptSignatureWindow 是回执签名时间戳与本机时钟的最大偏差（§6.5：5 分钟时间窗）。
const ReceiptSignatureWindow = 5 * time.Minute

// signaturePrefix 是 X-Signature 可选前缀（sha256=<hex>）。
const signaturePrefix = "sha256="

// VerifyReceiptSignature 校验 X-Signature = hex(HMAC-SHA256(secret, timestamp + "." + body))，timestamp 为 Unix 秒，
// 与 now 偏差超过 ReceiptSignatureWindow 视为过期；比较用 hmac.Equal（常量时间）。不通过 → ErrReceiptAuth。
func VerifyReceiptSignature(secret []byte, timestamp, signature string, body []byte, now time.Time) error {
	if len(secret) == 0 {
		return fmt.Errorf("%w: secret not configured", ErrReceiptAuth)
	}
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad timestamp", ErrReceiptAuth)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > ReceiptSignatureWindow || d < -ReceiptSignatureWindow {
		return fmt.Errorf("%w: timestamp outside window", ErrReceiptAuth)
	}
	got, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(signature), signaturePrefix))
	if err != nil || len(got) != sha256.Size {
		return fmt.Errorf("%w: bad signature encoding", ErrReceiptAuth)
	}
	if !hmac.Equal(got, receiptMAC(secret, timestamp, body)) {
		return fmt.Errorf("%w: signature mismatch", ErrReceiptAuth)
	}
	return nil
}

// SignReceipt 返回 hex(HMAC-SHA256(secret, timestamp + "." + body))（模拟平台与测试用）。
func SignReceipt(secret []byte, timestamp string, body []byte) string {
	return hex.EncodeToString(receiptMAC(secret, timestamp, body))
}

func receiptMAC(secret []byte, timestamp string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}

// WebhookInput 是一次回执 webhook 请求（controller 原样转交，Body 为原始字节，签名按它算）。
type WebhookInput struct {
	FarmID    uint64 // 路径 farm_id
	OrderID   uint64 // 路径 order_id
	Timestamp string // X-Timestamp
	Signature string // X-Signature
	Body      []byte
	Route     string
	RequestID string
}

// WebhookStatus 是 webhook 处理结论。
type WebhookStatus string

const (
	WebhookProcessed WebhookStatus = "PROCESSED" // 已入库（含 STALE / LATE / CONFLICT）
	WebhookReplayed  WebhookStatus = "REPLAYED"  // 同一回执已处理过
	WebhookUnmatched WebhookStatus = "UNMATCHED" // 反查不到归属，原文存证 + 审计，转人工
)

// WebhookResult 是 webhook 的响应体。
type WebhookResult struct {
	Status      WebhookStatus         `json:"status"`
	ReceiptID   string                `json:"receipt_id,omitempty"`
	Outcome     model.ReceiptOutcome  `json:"effect,omitempty"`
	OrderStatus model.WorkOrderStatus `json:"order_status,omitempty"`
}

// webhookBody 是回执 webhook 请求体（设备平台按标准回执推送）。
type webhookBody struct {
	ReceiptID   string    `json:"receipt_id"`
	DeviceType  string    `json:"device_type"`
	DeviceJobID string    `json:"device_job_id"`
	Status      string    `json:"status"`
	FailureKind string    `json:"failure_kind"`
	DoneAreaMu  *float64  `json:"done_area_mu"`
	DeviceTS    time.Time `json:"device_ts"`
	Code        string    `json:"code"`
}

func (b webhookBody) receipt() model.DeviceReceipt {
	return model.DeviceReceipt{Status: model.AttemptStatus(b.Status), DeviceJobID: b.DeviceJobID, DoneAreaMu: b.DoneAreaMu,
		DeviceTime: b.DeviceTS.UTC(), FailureKind: model.FailureKind(b.FailureKind), Code: b.Code}
}

// Webhook 处理设备回执推送（§6.5 L4-B 第 1 条）：验签（ErrReceiptAuth → 401）→ 解析与 DeviceReceipt.Validate（ErrBadInput → 400）→
// SystemScope 按 (device_type, device_job_id) 反查尝试：与路径 farm / order 不一致 → CROSS_FARM_DENIED 审计 + ErrForbidden（403）；
// 反查不到 → ErrReceiptUnknownJob（503，平台重投）；无任务号、或 device_ts 超 10 分钟仍反查不到 → 原文存证 +
// RECEIPT_UNMATCHED 审计（0,0）+ 告警，WebhookUnmatched（202）。反查到即交 Process（source=WEBHOOK）。
func (s *OrderOpsService) Webhook(ctx context.Context, in WebhookInput) (WebhookResult, error) {
	now := s.d.Clock.Now()
	if err := VerifyReceiptSignature(s.d.Secret, in.Timestamp, in.Signature, in.Body, now); err != nil {
		s.d.Logger.WarnContext(ctx, "receipt webhook: signature rejected", "path_farm_id", in.FarmID, "path_order_id", in.OrderID,
			"request_id", in.RequestID, "err", err)
		return WebhookResult{}, err
	}
	var b webhookBody
	if err := json.Unmarshal(in.Body, &b); err != nil {
		return WebhookResult{}, fmt.Errorf("%w: receipt body: %w", ErrBadInput, err)
	}
	rc := b.receipt()
	switch {
	case in.FarmID == 0 || in.OrderID == 0:
		return WebhookResult{}, fmt.Errorf("%w: path farm_id / order_id required", ErrBadInput)
	case b.DeviceType == "":
		return WebhookResult{}, fmt.Errorf("%w: device_type required", ErrBadInput)
	case b.DeviceTS.IsZero():
		return WebhookResult{}, fmt.Errorf("%w: device_ts required", ErrBadInput)
	case len(b.ReceiptID) > receiptIDMax:
		return WebhookResult{}, fmt.Errorf("%w: receipt_id longer than %d", ErrBadInput, receiptIDMax)
	}
	if err := rc.Validate(); err != nil {
		return WebhookResult{}, fmt.Errorf("%w: %w", ErrBadInput, err)
	}
	receiptID := ReceiptIDOf(b.ReceiptID, model.SourceWebhook, rc)
	if rc.DeviceJobID == "" {
		return s.unmatched(ctx, in, b, receiptID, "no device_job_id")
	}
	owner, err := s.d.Lookup.ByJob(ctx, receiptLookupScope, b.DeviceType, rc.DeviceJobID)
	if errors.Is(err, model.ErrNotFound) {
		if now.Sub(b.DeviceTS) > receiptLateFor {
			return s.unmatched(ctx, in, b, receiptID, "job not found after "+receiptLateFor.String())
		}
		return WebhookResult{}, fmt.Errorf("%w: %s/%s", ErrReceiptUnknownJob, b.DeviceType, rc.DeviceJobID)
	}
	if err != nil {
		return WebhookResult{}, fmt.Errorf("service: receipt lookup: %w", err)
	}
	if owner.FarmID != in.FarmID || owner.OrderID != in.OrderID {
		s.d.Denials.Deny(ctx, AccessDenial{FarmID: in.FarmID, Code: DenyResource,
			Target: "order:" + strconv.FormatUint(in.OrderID, 10), Route: in.Route, RequestID: in.RequestID})
		return WebhookResult{}, fmt.Errorf("%w: receipt job %s/%s belongs to another order", ErrForbidden, b.DeviceType, rc.DeviceJobID)
	}
	res, err := s.Process(ctx, ReceiptInput{Ref: model.OrderRef{TenantID: owner.TenantID, FarmID: owner.FarmID, OrderID: owner.OrderID},
		AttemptID: owner.ID, ReceiptID: receiptID, Receipt: rc, Source: model.SourceWebhook, Raw: in.Body})
	if err != nil {
		return WebhookResult{}, err
	}
	out := WebhookResult{Status: WebhookProcessed, ReceiptID: receiptID, Outcome: res.Outcome, OrderStatus: res.OrderStatus}
	if res.Replayed {
		out.Status = WebhookReplayed
	}
	return out, nil
}

// unmatchedSummary 是 RECEIPT_UNMATCHED 审计的 payload（控制字段，不含原文）。
type unmatchedSummary struct {
	PathFarmID  uint64 `json:"path_farm_id"`
	PathOrderID uint64 `json:"path_order_id"`
	DeviceType  string `json:"device_type"`
	DeviceJobID string `json:"device_job_id,omitempty"`
	Status      string `json:"status"`
	ReceiptID   string `json:"receipt_id"`
	Reason      string `json:"reason"`
	RequestID   string `json:"request_id,omitempty"`
}

// unmatched 把无法匹配的回执转人工：原文存证（路径农场能唯一定位租户时）、audit_log(0,0,RECEIPT_UNMATCHED)、告警。
// 审计写失败返回错误（平台重投），避免证据丢失。
func (s *OrderOpsService) unmatched(ctx context.Context, in WebhookInput, b webhookBody, receiptID, reason string) (WebhookResult, error) {
	var rawID *uint64
	tenants, err := s.d.Tenants.FarmTenants(ctx, in.FarmID)
	switch {
	case err != nil:
		s.d.Logger.WarnContext(ctx, "receipt unmatched: farm tenant lookup", "path_farm_id", in.FarmID, "err", err)
	case len(tenants) == 1:
		id, err := s.archiveReceipt(ctx, model.OrderRef{TenantID: tenants[0], FarmID: in.FarmID, OrderID: in.OrderID}, receiptID, in.Body)
		if err != nil {
			return WebhookResult{}, err
		}
		rawID = &id
	default:
		s.d.Logger.WarnContext(ctx, "receipt unmatched: raw not archived, farm tenant ambiguous", "path_farm_id", in.FarmID,
			"tenants", len(tenants))
	}
	payload, err := json.Marshal(unmatchedSummary{PathFarmID: in.FarmID, PathOrderID: in.OrderID, DeviceType: b.DeviceType,
		DeviceJobID: b.DeviceJobID, Status: b.Status, ReceiptID: receiptID, Reason: reason, RequestID: in.RequestID})
	if err != nil {
		return WebhookResult{}, fmt.Errorf("service: encode RECEIPT_UNMATCHED: %w", err)
	}
	rec := repository.NewAuditRecord(model.Scope{}, nil, model.Actor{Type: model.ActorDevice}, model.AuditReceiptUnmatched,
		"order:"+strconv.FormatUint(in.OrderID, 10), payload, model.AuditResultFailed, s.d.Clock.Now())
	rec.RawObjectID = rawID
	if err := s.d.Audit.Append(ctx, rec); err != nil {
		return WebhookResult{}, fmt.Errorf("service: audit RECEIPT_UNMATCHED: %w", err)
	}
	s.d.Logger.ErrorContext(ctx, "receipt unmatched, needs manual handling", "alert", alertReceiptUnmatched, "reason", reason,
		"path_farm_id", in.FarmID, "path_order_id", in.OrderID, "device_type", b.DeviceType, "device_job_id", b.DeviceJobID,
		"receipt_id", receiptID, "raw_archived", rawID != nil)
	return WebhookResult{Status: WebhookUnmatched, ReceiptID: receiptID}, nil
}
