// Package controller 是 HTTP 层（gin）：鉴权中间件、参数校验、错误码映射；只调 service。
// Scope 放进 c.Request.Context()，service 只依赖 context.Context、不 import gin。
package controller

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

const bearerPrefix = "Bearer "

// Authenticator 是中间件需要的鉴权能力，service.AuthService 实现（P1 冻结接缝，签名只加不改）。
type Authenticator interface {
	Authenticate(token string) (uint64, error)
	ResolveScope(ctx context.Context, userID, farmID uint64) (model.Scope, error)
	ResolvePlatformScope(ctx context.Context, userID uint64) (model.Scope, error)
}

// errorBody 是统一错误响应。
type errorBody struct {
	Error string `json:"error"`
}

// FarmAuth 校验 Bearer token 并按路径 :farm_id 解析 Scope（每次查 user_role，不缓存）：无 token / 验签失败 → 401；
// 农场不在授权内，或 token 的 tenant_id 与农场归属不符（platform_admin 除外）→ 403 SCOPE_DENIED 并审计。
// 请求体中的 farm_id 一律忽略。
// 例外：只有 SSE 路由（EventsPath）在没有 Authorization 头时接受 ?access_token=（EventSource 无法带头）。
func FarmAuth(g Guard) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := identify(c, g, requestToken(c, c.FullPath() == EventsPath))
		if !ok {
			return
		}
		farmID, err := strconv.ParseUint(c.Param("farm_id"), 10, 64)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad farm_id")
			return
		}
		establishScope(c, g, id, "farm:"+strconv.FormatUint(farmID, 10), func(ctx context.Context) (model.Scope, error) {
			scope, err := g.Auth.ResolveScope(ctx, id.UserID, farmID)
			if err == nil && id.TenantID != scope.TenantID && !scope.HasRole(model.RolePlatformAdmin) {
				return model.Scope{}, &tenantMismatchError{owner: scope.TenantID}
			}
			return scope, err
		})
	}
}

// tenantMismatchError 是 token 租户与路径农场真实归属不符（属于 ErrForbidden）；owner 写进审计 summary.owner_tenant_id。
type tenantMismatchError struct{ owner uint64 }

func (e *tenantMismatchError) Error() string {
	return "token tenant does not own farm (owner tenant " + strconv.FormatUint(e.owner, 10) + ")"
}

func (e *tenantMismatchError) Unwrap() error { return service.ErrForbidden }

// requestToken 取 Authorization: Bearer 头中的 token；没有该头且 allowQuery 时取 ?access_token=（仅 SSE）。
func requestToken(c *gin.Context, allowQuery bool) string {
	if token, ok := strings.CutPrefix(c.GetHeader("Authorization"), bearerPrefix); ok {
		return strings.TrimSpace(token)
	}
	if allowQuery {
		return strings.TrimSpace(c.Query("access_token"))
	}
	return ""
}

// identify 校验 token；空 token / 验签失败 → 401 并返回 false。
func identify(c *gin.Context, g Guard, token string) (service.Identity, bool) {
	id, err := g.Auth.Identify(token)
	if err != nil {
		abort(c, http.StatusUnauthorized, "unauthenticated")
		return service.Identity{}, false
	}
	return id, true
}

// establishScope 是 FarmAuth / PlatformAuth 共用的「解析 Scope → 映射错误 → 放入 ctx」：
// resolve 返回 ErrForbidden → 403 SCOPE_DENIED 并审计（Scope 未建立，平台级事件写 (0,0)，target 如 farm:2 / platform）；
// 其余错误记日志 → 500。
func establishScope(c *gin.Context, g Guard, id service.Identity, target string,
	resolve func(ctx context.Context) (model.Scope, error)) {
	ctx := c.Request.Context()
	scope, err := resolve(ctx)
	if err != nil {
		if !errors.Is(err, service.ErrForbidden) {
			g.logger().ErrorContext(ctx, "resolve scope", "user_id", id.UserID, "target", target, "err", err)
			abort(c, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
			return
		}
		d := denial(c, model.Scope{UserID: id.UserID}, service.DenyScope, "", target)
		var tm *tenantMismatchError
		if errors.As(err, &tm) {
			d.OwnerTenantID = tm.owner
		}
		g.Denials.Deny(ctx, d)
		abort(c, http.StatusForbidden, codeScopeDenied)
		return
	}
	c.Request = c.Request.WithContext(service.WithScope(ctx, scope))
	c.Next()
}

// pathFarmID 返回路径 :farm_id（非法时为 0，只用于审计）。
func pathFarmID(c *gin.Context) uint64 {
	id, err := strconv.ParseUint(c.Param("farm_id"), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func abort(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, errorBody{Error: msg})
}

// Ping 返回当前请求解析出的 Scope（L1 鉴权自检接口）。
func Ping(c *gin.Context) {
	scope, ok := service.ScopeFrom(c.Request.Context())
	if !ok {
		abort(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	c.JSON(http.StatusOK, scope)
}

// NewRouter 组装路由：资源一律挂在 /api/v1/farms/:farm_id/ 下并经 RequestID + FarmAuth + Authorize（权限矩阵）；
// mounts 追加各层路由（如 ApprovalRoutes、RunRoutes），新路由须在 routeOps 登记所需操作。
func NewRouter(g Guard, ask Asker, mounts ...Mount) *gin.Engine {
	g.Logger = g.logger()
	r := gin.New()
	r.Use(gin.Recovery())
	farms := r.Group(farmsPrefix, RequestID(), FarmAuth(g), Authorize(g))
	farms.GET("/ping", Ping)
	farms.POST("/ask", AskHandler(ask))
	for _, m := range mounts {
		m(farms)
	}
	return r
}
