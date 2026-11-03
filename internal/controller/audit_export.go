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

// P1 L7 审计导出路由（§6.8 技术实现第 5 条，权限 audit.export：仅 auditor / platform_admin）。

// AuditExporter 是导出路由需要的能力，service.AuditExportService 实现。
type AuditExporter interface {
	Request(ctx context.Context, scope model.Scope, req service.AuditExportRequest) (service.AuditExportStatus, error)
	Status(ctx context.Context, scope model.Scope, id uint64) (service.AuditExportStatus, error)
}

// AuditExportRoutes 挂载 POST /audit/exports 与 GET /audit/exports/:export_id。
func AuditExportRoutes(e AuditExporter) Mount {
	return func(farms *gin.RouterGroup) {
		farms.POST("/audit/exports", AuditExportRequestHandler(e))
		farms.GET("/audit/exports/:export_id", AuditExportStatusHandler(e))
	}
}

// AuditExportRequestHandler 是 POST …/audit/exports：请求体 {from, to, request_id}（RFC3339，[from, to)），
// 202 返回 {export_id, state}；同 request_id 重放返回同一导出。区间非法 / 超跨度 → 400。
func AuditExportRequestHandler(e AuditExporter) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var req service.AuditExportRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		st, err := e.Request(c.Request.Context(), scope, req)
		if err != nil {
			abortErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, st)
	}
}

// AuditExportStatusHandler 是 GET …/audit/exports/:export_id：进度；DONE 时带 5 分钟预签名 URL（已写 RAW_READ）。
// 他场导出 → 404；导出文件已过保留期 → 410。
func AuditExportStatusHandler(e AuditExporter) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id, err := strconv.ParseUint(c.Param("export_id"), 10, 64)
		if err != nil || id == 0 {
			abort(c, http.StatusBadRequest, "bad export_id")
			return
		}
		st, err := e.Status(c.Request.Context(), scope, id)
		if errors.Is(err, model.ErrRawExpired) {
			abort(c, http.StatusGone, model.ErrRawExpired.Error())
			return
		}
		if err != nil {
			abortErr(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, st)
	}
}
