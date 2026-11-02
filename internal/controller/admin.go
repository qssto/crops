package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// AdminPrefix 是平台管理路由组（不挂在 /farms/:farm_id 下，平台 Scope 的 tenant_id = farm_id = 0）。
const AdminPrefix = "/api/v1/admin"

// FlagAdmin 是开关管理需要的能力，service.FlagService 实现。
type FlagAdmin interface {
	Set(ctx context.Context, actor model.Scope, in service.FlagSetInput) (service.FlagSetResult, error)
	List(ctx context.Context, actor model.Scope, key model.FlagKey) ([]model.FlagSetting, error)
}

// flagSetRequest 是 PUT 请求体；作用域只取路径。
type flagSetRequest struct {
	Value        json.RawMessage `json:"value"`
	Comment      string          `json:"comment"`
	ExpectRowVer *int            `json:"expect_row_ver"`
}

// MountAdmin 在 r 上挂 /api/v1/admin 路由组：RequestID + PlatformAuth（P1 §6.1 功能任务 3）。
// 合并 L1-A 后由调度方在组上追加 flag.write 操作检查。
func MountAdmin(r *gin.Engine, a Authenticator, f FlagAdmin) {
	admin := r.Group(AdminPrefix, RequestID(), PlatformAuth(a))
	admin.PUT("/flags/:flag_key/scopes/:tenant_id/:farm_id", FlagSetHandler(f))
	admin.GET("/flags/:flag_key", FlagListHandler(f))
}

// PlatformAuth 校验 Bearer token 并解析平台 Scope：无 token / 验签失败 → 401，无 (0,0) 绑定 → 403。
func PlatformAuth(a Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), bearerPrefix)
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		uid, err := a.Authenticate(strings.TrimSpace(token))
		if err != nil {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		scope, err := a.ResolvePlatformScope(c.Request.Context(), uid)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, service.ErrForbidden) {
				status = http.StatusForbidden
			}
			abort(c, status, http.StatusText(status))
			return
		}
		c.Request = c.Request.WithContext(service.WithScope(c.Request.Context(), scope))
		c.Next()
	}
}

// FlagSetHandler 是 PUT /api/v1/admin/flags/:flag_key/scopes/:tenant_id/:farm_id（GLOBAL 用 0/0，TENANT 用 t/0）。
func FlagSetHandler(f FlagAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		tenantID, err := strconv.ParseUint(c.Param("tenant_id"), 10, 64)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad tenant_id")
			return
		}
		farmID, err := strconv.ParseUint(c.Param("farm_id"), 10, 64)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad farm_id")
			return
		}
		var req flagSetRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		res, err := f.Set(c.Request.Context(), scope, service.FlagSetInput{Key: model.FlagKey(c.Param("flag_key")),
			TenantID: tenantID, FarmID: farmID, Value: req.Value, Comment: req.Comment, ExpectRowVer: req.ExpectRowVer})
		if err != nil {
			abortFlagErr(c, err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// FlagListHandler 是 GET /api/v1/admin/flags/:flag_key：灰度档位一览。
func FlagListHandler(f FlagAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		rows, err := f.List(c.Request.Context(), scope, model.FlagKey(c.Param("flag_key")))
		if err != nil {
			abortFlagErr(c, err)
			return
		}
		if rows == nil {
			rows = []model.FlagSetting{}
		}
		c.JSON(http.StatusOK, gin.H{"flag_key": c.Param("flag_key"), "settings": rows})
	}
}

// flagCodes 是开关错误码到 HTTP 状态的映射；错误码原样回给客户端。
var flagCodes = []struct {
	err    error
	status int
}{
	{service.ErrRoleInsufficient, http.StatusForbidden},
	{service.ErrFlagUnknown, http.StatusNotFound},
	{service.ErrFlagValue, http.StatusBadRequest},
	{service.ErrFlagScope, http.StatusBadRequest},
	{service.ErrFlagConflict, http.StatusConflict},
	{service.ErrFlagPrecondition, http.StatusConflict},
}

// abortFlagErr 先映射开关错误码，其余交给 abortErr。
func abortFlagErr(c *gin.Context, err error) {
	for _, fc := range flagCodes {
		if errors.Is(err, fc.err) {
			abort(c, fc.status, fc.err.Error())
			return
		}
	}
	abortErr(c, err)
}
