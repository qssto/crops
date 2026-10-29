// Package controller 是 HTTP 层（gin）：鉴权中间件、参数校验、错误码映射；只调 service。
// Scope 放进 c.Request.Context()，service 只依赖 context.Context、不 import gin。
package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

const bearerPrefix = "Bearer "

// Authenticator 是中间件需要的鉴权能力，service.AuthService 实现。
type Authenticator interface {
	Authenticate(token string) (uint64, error)
	ResolveScope(userID, farmID uint64) (model.Scope, error)
}

// errorBody 是统一错误响应。
type errorBody struct {
	Error string `json:"error"`
}

// FarmAuth 校验 Bearer token 并按路径 :farm_id 解析 Scope：无 token / 验签失败 → 401，
// 农场不在授权内 → 403。请求体中的 farm_id 一律忽略。
// 例外：只有 SSE 路由（EventsPath）在没有 Authorization 头时接受 ?access_token=（EventSource 无法带头）。
func FarmAuth(a Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := strings.CutPrefix(c.GetHeader("Authorization"), bearerPrefix)
		if !ok {
			token = ""
			if c.FullPath() == EventsPath {
				token = c.Query("access_token")
			}
		}
		uid, err := a.Authenticate(strings.TrimSpace(token))
		if err != nil {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		farmID, err := strconv.ParseUint(c.Param("farm_id"), 10, 64)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad farm_id")
			return
		}
		scope, err := a.ResolveScope(uid, farmID)
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

// NewRouter 组装路由：资源一律挂在 /api/v1/farms/:farm_id/ 下并经 RequestID + FarmAuth；mounts 追加各层路由（如 ApprovalRoutes、RunRoutes）。
func NewRouter(a Authenticator, ask Asker, mounts ...Mount) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	farms := r.Group("/api/v1/farms/:farm_id", RequestID(), FarmAuth(a))
	farms.GET("/ping", Ping)
	farms.POST("/ask", AskHandler(ask))
	for _, m := range mounts {
		m(farms)
	}
	return r
}
