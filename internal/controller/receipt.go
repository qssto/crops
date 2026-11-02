package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// 设备回执 webhook（P1-L4-B）：挂在根路由，不经用户鉴权，自带 HMAC 签名（X-Signature / X-Timestamp）。

// ReceiptPath 是回执 webhook 路由模板。
const ReceiptPath = "/api/v1/farms/:farm_id/orders/:order_id/receipts"

// 回执请求头。
const (
	HeaderSignature = "X-Signature"
	HeaderTimestamp = "X-Timestamp"
)

// maxReceiptBody 是回执请求体上限（64 KiB），超出 → 413。
const maxReceiptBody = 64 << 10

// ReceiptHandlerSvc 是回执 webhook 需要的能力，service.OrderOpsService 实现。
type ReceiptHandlerSvc interface {
	Webhook(ctx context.Context, in service.WebhookInput) (service.WebhookResult, error)
}

// ReceiptRoutes 挂载回执 webhook（EngineMount）；挂 RequestID 中间件，便于审计关联。
func ReceiptRoutes(s ReceiptHandlerSvc) EngineMount {
	return func(r *gin.Engine) {
		r.POST(ReceiptPath, RequestID(), ReceiptHandler(s))
	}
}

// ReceiptHandler 是 POST /api/v1/farms/:farm_id/orders/:order_id/receipts：原始字节交 service 验签与处理。
// 401 签名错误 / 过期；400 参数或回执非法；403 回执归属与路径不符；503 反查不到任务（平台重投）或工单忙；
// 202 无法匹配（已存证转人工）；200 已处理（含 STALE / LATE / CONFLICT）或重放。
func ReceiptHandler(s ReceiptHandlerSvc) gin.HandlerFunc {
	return func(c *gin.Context) {
		farmID, err1 := strconv.ParseUint(c.Param("farm_id"), 10, 64)
		orderID, err2 := strconv.ParseUint(c.Param("order_id"), 10, 64)
		if err1 != nil || err2 != nil || farmID == 0 || orderID == 0 {
			abort(c, http.StatusBadRequest, "bad farm_id / order_id")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxReceiptBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				abort(c, http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge))
				return
			}
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		res, err := s.Webhook(c.Request.Context(), service.WebhookInput{FarmID: farmID, OrderID: orderID,
			Timestamp: c.GetHeader(HeaderTimestamp), Signature: c.GetHeader(HeaderSignature), Body: body,
			Route: c.Request.Method + " " + c.FullPath(), RequestID: service.RequestIDFrom(c.Request.Context())})
		if err != nil {
			abortReceiptErr(c, err)
			return
		}
		status := http.StatusOK
		if res.Status == service.WebhookUnmatched {
			status = http.StatusAccepted
		}
		c.JSON(status, res)
	}
}

// receiptCodes 是回执错误到 HTTP 状态的映射；错误码原样回给平台（不含细节）。
var receiptCodes = []struct {
	err    error
	status int
}{
	{service.ErrReceiptAuth, http.StatusUnauthorized},
	{service.ErrBadInput, http.StatusBadRequest},
	{service.ErrForbidden, http.StatusForbidden},
	{service.ErrReceiptUnknownJob, http.StatusServiceUnavailable},
	{service.ErrOrderBusy, http.StatusServiceUnavailable},
	{model.ErrConcurrentUpdate, http.StatusServiceUnavailable},
}

func abortReceiptErr(c *gin.Context, err error) {
	for _, rc := range receiptCodes {
		if errors.Is(err, rc.err) {
			if rc.status == http.StatusServiceUnavailable {
				c.Header("Retry-After", "5")
			}
			abort(c, rc.status, http.StatusText(rc.status))
			return
		}
	}
	abortInternal(c, "receipt webhook failed", err)
}
