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

// P2 L8 归因问题处置（§6.9 技术实现 ② attribution_issue；平台路由，权限 attribution.write，不做 amis 页面）。

// IssueAdmin 是处置更新能力，service.AttributionIssueService 实现。
type IssueAdmin interface {
	Patch(ctx context.Context, actor model.Scope, id uint64, p model.AttributionPatch) (model.AttributionIssue, error)
}

// MountAttributionAdmin 挂载 PATCH /api/v1/admin/attribution-issues/:id（平台 Scope + attribution.write）。
func MountAttributionAdmin(r *gin.Engine, g Guard, s IssueAdmin) {
	admin := r.Group(AdminPrefix, RequestID(), PlatformAuth(g), RequireOps(g, service.OpAttributionWrite))
	admin.PATCH("/attribution-issues/:id", IssuePatchHandler(s))
}

// AttributionAdminRoutes 是 MountAttributionAdmin 的 EngineMount 形式（app 装配用，app 不 import gin）。
func AttributionAdminRoutes(g Guard, s IssueAdmin) EngineMount {
	return func(r *gin.Engine) { MountAttributionAdmin(r, g, s) }
}

// IssuePatchHandler 是 PATCH …/attribution-issues/:id：200 返回更新后的行；入参非法 → 400，行不存在 → 404，
// row_ver 不符 → 409 ISSUE_ROW_VER_CONFLICT。
func IssuePatchHandler(s IssueAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil || id == 0 {
			abort(c, http.StatusBadRequest, "bad id")
			return
		}
		var p model.AttributionPatch
		if err := c.ShouldBindJSON(&p); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		got, err := s.Patch(c.Request.Context(), scope, id, p)
		switch {
		case err == nil:
			c.JSON(http.StatusOK, got)
		case errors.Is(err, service.ErrIssueConflict):
			abort(c, http.StatusConflict, service.ErrIssueConflict.Error())
		case errors.Is(err, model.ErrAttributionPatch):
			abort(c, http.StatusBadRequest, model.ErrAttributionPatch.Error())
		case errors.Is(err, service.ErrRoleInsufficient):
			abort(c, http.StatusForbidden, service.ErrRoleInsufficient.Error())
		default:
			abortErr(c, err)
		}
	}
}
