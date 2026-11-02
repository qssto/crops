package controller

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// P1 L5 待审批详情路由（advice.read）。

// ApprovalDetailer 是待审批详情能力，service.ApprovalDetailService 实现。
type ApprovalDetailer interface {
	Detail(ctx context.Context, scope model.Scope, ticketID uint64) (service.ApprovalDetail, error)
}

// ApprovalDetailRoutes 挂载 GET /approvals/:ticket_id。
func ApprovalDetailRoutes(d ApprovalDetailer) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/approvals/:ticket_id", ApprovalDetailHandler(d))
	}
}

// ApprovalDetailHandler 是 GET /api/v1/farms/:farm_id/approvals/:ticket_id：票据 + 完整快照（risk_reasons / highlights / diff）
// + PRE_APPROVAL 与最新一轮 PRE_DISPATCH 校验。票据不在本农场 → 403 + 审计。
func ApprovalDetailHandler(d ApprovalDetailer) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		ticketID, err := strconv.ParseUint(c.Param("ticket_id"), 10, 64)
		if err != nil || ticketID == 0 {
			abort(c, http.StatusBadRequest, "bad ticket_id")
			return
		}
		res, err := d.Detail(c.Request.Context(), scope, ticketID)
		if err != nil {
			abortScoped(c, err, "ticket:"+strconv.FormatUint(ticketID, 10))
			return
		}
		c.JSON(http.StatusOK, res)
	}
}
