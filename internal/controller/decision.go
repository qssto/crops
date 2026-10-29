package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// Decider 是审批入口需要的能力，service.DecisionService 实现。
type Decider interface {
	Pending(ctx context.Context, scope model.Scope) ([]service.PendingTicket, error)
	Decide(ctx context.Context, scope model.Scope, in service.DecideInput) (service.DecideResult, error)
}

// Mount 在 /api/v1/farms/:farm_id 分组上追加路由（分组已带 RequestID + FarmAuth）。
type Mount func(farms *gin.RouterGroup)

// ApprovalRoutes 挂载待审批列表与审批决策（P0 §6.6 / §6.7）。
func ApprovalRoutes(d Decider) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/approvals", PendingHandler(d))
		farms.POST("/approvals/:ticket_id/decide", DecideHandler(d))
	}
}

// decideRequest 是 POST /approvals/:ticket_id/decide 请求体；request_id 只取 X-Request-Id 头。
type decideRequest struct {
	Decision       string `json:"decision"`
	SnapshotDigest string `json:"snapshot_digest"`
	Comment        string `json:"comment"`
}

// PendingHandler 是 GET /api/v1/farms/:farm_id/approvals：本农场 OPEN 票列表。
func PendingHandler(d Decider) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		rows, err := d.Pending(c.Request.Context(), scope)
		if err != nil {
			abortDecideErr(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"tickets": rows})
	}
}

// DecideHandler 是 POST /api/v1/farms/:farm_id/approvals/:ticket_id/decide：X-Request-Id 必填（幂等键），
// 不接受中间件生成的随机 id。
func DecideHandler(d Decider) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if c.GetHeader(HeaderRequestID) == "" {
			abort(c, http.StatusBadRequest, "missing "+HeaderRequestID)
			return
		}
		ticketID, err := strconv.ParseUint(c.Param("ticket_id"), 10, 64)
		if err != nil || ticketID == 0 {
			abort(c, http.StatusBadRequest, "bad ticket_id")
			return
		}
		var req decideRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		res, err := d.Decide(c.Request.Context(), scope, service.DecideInput{TicketID: ticketID,
			RequestID: service.RequestIDFrom(c.Request.Context()), Decision: req.Decision,
			SnapshotDigest: req.SnapshotDigest, Comment: req.Comment})
		if err != nil {
			abortDecideErr(c, err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// decideCodes 是审批业务错误码到 HTTP 状态的映射；错误码原样回给客户端。
var decideCodes = []struct {
	err    error
	status int
}{
	{service.ErrTicketClosed, http.StatusConflict},
	{service.ErrTicketExpired, http.StatusConflict},
	{service.ErrSnapshotStale, http.StatusConflict},
	{service.ErrRequestInFlight, http.StatusConflict},
	{service.ErrRequestConflict, http.StatusConflict},
	{service.ErrOrderBusy, http.StatusConflict},
	{service.ErrRoleInsufficient, http.StatusForbidden},
	{service.ErrSameApprover, http.StatusForbidden},
}

// abortDecideErr 先映射审批错误码，其余交给 abortErr。
func abortDecideErr(c *gin.Context, err error) {
	for _, dc := range decideCodes {
		if errors.Is(err, dc.err) {
			abort(c, dc.status, dc.err.Error())
			return
		}
	}
	abortErr(c, err)
}
