package controller

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// RawReader 是读原文入口需要的能力，service.AuditService 实现。
type RawReader interface {
	ReadRaw(ctx context.Context, scope model.Scope, runID, rawID uint64) (service.RawLink, error)
}

// AuditRoutes 挂载原文读取（P0 §6.8 功能任务 7）。
func AuditRoutes(r RawReader) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/runs/:run_id/raw/:raw_id", RawHandler(r))
	}
}

// RawHandler 是 GET /api/v1/farms/:farm_id/runs/:run_id/raw/:raw_id：返回 5 分钟预签名 URL；
// 角色不足 / 不属于本农场 / 跨 run 一律 403（已写 RAW_READ/DENIED）。
func RawHandler(r RawReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		runID, err := strconv.ParseUint(c.Param("run_id"), 10, 64)
		if err != nil || runID == 0 {
			abort(c, http.StatusBadRequest, "bad run_id")
			return
		}
		rawID, err := strconv.ParseUint(c.Param("raw_id"), 10, 64)
		if err != nil || rawID == 0 {
			abort(c, http.StatusBadRequest, "bad raw_id")
			return
		}
		link, err := r.ReadRaw(c.Request.Context(), scope, runID, rawID)
		if err != nil {
			abortErr(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, link)
	}
}
