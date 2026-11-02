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

// 作业单运维路由（P1-L4-B）：中止、人工对账、待对账列表。权限由 routeOps 判定（order.cancel / order.reconcile / advice.read）。

// OrderOps 是运维路由需要的能力，service.OrderOpsService 实现。
type OrderOps interface {
	Cancel(ctx context.Context, scope model.Scope, orderID uint64) (service.CancelResult, error)
	Reconcile(ctx context.Context, scope model.Scope, in service.ReconcileInput) (service.ReconcileResult, error)
	PendingReconcile(ctx context.Context, scope model.Scope) ([]model.DispatchAttempt, error)
}

// OrderOpsRoutes 挂载中止 / 对账 / 待对账列表。
func OrderOpsRoutes(s OrderOps) Mount {
	return func(farms *gin.RouterGroup) {
		farms.POST("/orders/:order_id/cancel", CancelHandler(s))
		farms.POST("/orders/:order_id/reconcile", ReconcileHandler(s))
		farms.GET("/reconcile/pending", PendingReconcileHandler(s))
	}
}

// pathOrderID 解析 :order_id；非法时回 400 并返回 false。
func pathOrderID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("order_id"), 10, 64)
	if err != nil || id == 0 {
		abort(c, http.StatusBadRequest, "bad order_id")
		return 0, false
	}
	return id, true
}

// CancelHandler 是 POST …/orders/:order_id/cancel：200 已中止或 202 已提交设备中止（pending）。
func CancelHandler(s OrderOps) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		orderID, ok := pathOrderID(c)
		if !ok {
			return
		}
		res, err := s.Cancel(c.Request.Context(), scope, orderID)
		if err != nil {
			abortOrderOpsErr(c, err, orderID)
			return
		}
		status := http.StatusOK
		if res.Pending {
			status = http.StatusAccepted
		}
		c.JSON(status, res)
	}
}

// ReconcileHandler 是 POST …/orders/:order_id/reconcile：请求体 {conclusion, failure_kind?, done_area_mu?, comment?}。
func ReconcileHandler(s OrderOps) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		orderID, ok := pathOrderID(c)
		if !ok {
			return
		}
		var in service.ReconcileInput
		if err := c.ShouldBindJSON(&in); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		in.OrderID = orderID
		res, err := s.Reconcile(c.Request.Context(), scope, in)
		if err != nil {
			abortOrderOpsErr(c, err, orderID)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// PendingReconcileHandler 是 GET …/reconcile/pending：本农场待对账的尝试（NO_RECEIPT、FAILED+UNKNOWN）。
func PendingReconcileHandler(s OrderOps) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		rows, err := s.PendingReconcile(c.Request.Context(), scope)
		if err != nil {
			abortErr(c, err)
			return
		}
		if rows == nil {
			rows = []model.DispatchAttempt{}
		}
		c.JSON(http.StatusOK, gin.H{"attempts": rows})
	}
}

// orderOpsCodes 是运维错误码到 HTTP 状态的映射；错误码原样回给客户端。
var orderOpsCodes = []struct {
	err    error
	status int
}{
	{service.ErrReconcileState, http.StatusConflict},
	{service.ErrCancelState, http.StatusConflict},
	{service.ErrOrderBusy, http.StatusConflict},
	{model.ErrConcurrentUpdate, http.StatusConflict},
	{model.ErrIllegalTransition, http.StatusConflict},
}

// abortOrderOpsErr 先映射业务码；工单不在本农场（service.ErrNotInScope）由 abortScoped 回 403 + 审计，不暴露是否存在。
func abortOrderOpsErr(c *gin.Context, err error, orderID uint64) {
	for _, oc := range orderOpsCodes {
		if errors.Is(err, oc.err) {
			abort(c, oc.status, oc.err.Error())
			return
		}
	}
	abortScoped(c, err, "order:"+strconv.FormatUint(orderID, 10))
}
