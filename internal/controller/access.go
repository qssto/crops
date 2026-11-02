package controller

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// 权限矩阵在 HTTP 侧的落点（P1 §6.2 功能任务 6、流程 5）：每条路由声明所需操作，Authorize 按矩阵判定，
// 未在 routeOps 登记的路由一律拒绝；拒绝与跨农场访问统一经 Denier 写 CROSS_FARM_DENIED 审计。

// Identifier 是中间件需要的鉴权能力：Authenticator（冻结接缝）+ 读 token 中的 tenant_id。service.AuthService 实现。
type Identifier interface {
	Authenticator
	Identify(token string) (service.Identity, error)
}

// Denier 记录访问拒绝（审计 + 日志 + 计数），service.DenialAuditor 实现。
type Denier interface {
	Deny(ctx context.Context, d service.AccessDenial)
}

// Permissions 是权限矩阵判定，*service.PermissionMatrix 实现。
type Permissions interface {
	Allows(op service.Operation, roles ...model.Role) bool
}

// Guard 是路由鉴权所需的依赖；Logger 为空取 slog.Default，其余必填。
type Guard struct {
	Auth    Identifier
	Perms   Permissions
	Denials Denier
	Logger  *slog.Logger
}

func (g Guard) logger() *slog.Logger {
	if g.Logger == nil {
		return slog.Default()
	}
	return g.Logger
}

// routeOps 是 /api/v1/farms/:farm_id 下每条路由（METHOD + 路由模板）所需的操作，满足其一即可。
// 审批决策的用药类 / 非用药类细分由 DecisionService 按作业类型再判（SPRAY 取 pesticide）。新增路由必须在此登记（测试双向核对）。
var routeOps = map[string][]service.Operation{
	"GET " + farmsPrefix + "/ping":                         {service.OpAdviceRead},
	"POST " + farmsPrefix + "/ask":                         {service.OpAsk},
	"GET " + farmsPrefix + "/runs/:run_id":                 {service.OpAdviceRead},
	"GET " + farmsPrefix + "/metrics":                      {service.OpAdviceRead},
	"GET " + EventsPath:                                    {service.OpAdviceRead},
	"GET " + farmsPrefix + "/runs/:run_id/raw/:raw_id":     {service.OpRawRead},
	"GET " + farmsPrefix + "/approvals":                    {service.OpAdviceRead},
	"POST " + farmsPrefix + "/approvals/:ticket_id/decide": {service.OpApprovalDecidePesticide, service.OpApprovalDecideOther},
	// P1-L4-B：中止 / 人工对账 / 待对账列表（列表复用读权限）。
	"POST " + farmsPrefix + "/orders/:order_id/cancel":    {service.OpOrderCancel},
	"POST " + farmsPrefix + "/orders/:order_id/reconcile": {service.OpOrderReconcile},
	"GET " + farmsPrefix + "/reconcile/pending":           {service.OpAdviceRead},
	// P1-L5：待审批详情（读权限）与驳回原因导出。
	"GET " + farmsPrefix + "/approvals/:ticket_id": {service.OpAdviceRead},
	"GET " + farmsPrefix + "/rejections":           {service.OpRejectionExport},
}

const farmsPrefix = "/api/v1/farms/:farm_id"

// 拒绝响应的错误码。
const (
	codeScopeDenied      = service.DenyScope
	codeRoleInsufficient = service.DenyRole
)

// guardKey 是 gin.Context 中 Guard 的键（handler 记录资源越界时取用）。
const guardKey = "agri-steward/guard"

// Authorize 按 routeOps 校验当前路由所需操作；须挂在 FarmAuth 之后。未登记路由 / 角色未授予 → 403 ROLE_INSUFFICIENT 并审计。
func Authorize(g Guard) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Request.Method + " " + c.FullPath()
		ops, ok := routeOps[key]
		if !ok {
			g.logger().ErrorContext(c.Request.Context(), "route has no permission entry", "route", key)
		}
		authorize(c, g, ops)
	}
}

// RequireOps 校验固定操作（供未挂在 farms 分组下的路由使用，如 admin 路由挂 flag.write）。
func RequireOps(g Guard, ops ...service.Operation) gin.HandlerFunc {
	return func(c *gin.Context) { authorize(c, g, ops) }
}

func authorize(c *gin.Context, g Guard, ops []service.Operation) {
	scope, ok := service.ScopeFrom(c.Request.Context())
	if !ok {
		abort(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if !slices.ContainsFunc(ops, func(op service.Operation) bool { return g.Perms.Allows(op, scope.Roles...) }) {
		g.Denials.Deny(c.Request.Context(), denial(c, scope, service.DenyRole, joinOps(ops), ""))
		abort(c, http.StatusForbidden, codeRoleInsufficient)
		return
	}
	c.Set(guardKey, g)
	c.Next()
}

// joinOps 把路由所需的全部操作以逗号连接（满足其一即可的路由，如 decide 的 pesticide / other），写进审计 summary.op。
func joinOps(ops []service.Operation) service.Operation {
	names := make([]string, len(ops))
	for i, op := range ops {
		names[i] = string(op)
	}
	return service.Operation(strings.Join(names, ","))
}

// denial 组装一次拒绝（路由、请求 id、路径 farm_id 取自请求）。
func denial(c *gin.Context, scope model.Scope, code string, op service.Operation, target string) service.AccessDenial {
	return service.AccessDenial{Scope: scope, UserID: scope.UserID, FarmID: pathFarmID(c), Code: code, Op: op, Target: target,
		Route: c.Request.Method + " " + c.FullPath(), RequestID: service.RequestIDFrom(c.Request.Context())}
}

// abortScoped 映射 service 错误：资源归属查询未命中（service.ErrNotInScope，以及 ErrForbidden 类拒绝）统一 403
// 并记 RESOURCE_NOT_IN_SCOPE（target 如 run:9），不暴露资源是否存在；归属已确认后关联数据缺失（裸 model.ErrNotFound）
// 是数据完整性错误，记日志 → 500，不当成越界。其余交给 abortErr。
func abortScoped(c *gin.Context, err error, target string) {
	switch {
	case errors.Is(err, service.ErrForbidden):
		recordDenial(c, service.DenyResource, "", target)
		abort(c, http.StatusForbidden, http.StatusText(http.StatusForbidden))
	case errors.Is(err, model.ErrNotFound):
		abortInternal(c, "scoped resource: related data missing", err, "target", target)
	default:
		abortErr(c, err)
	}
}

// recordDenial 在 handler 内记一次拒绝（Guard 由 Authorize 放入 gin.Context）。取不到 Guard / Scope 是装配错误：
// 记 Error 日志（审计缺失须可见），响应仍由调用方决定。
func recordDenial(c *gin.Context, code string, op service.Operation, target string) {
	ctx := c.Request.Context()
	g, ok := guardFrom(c)
	if !ok {
		slog.Default().ErrorContext(ctx, "access denial not audited: guard missing", "code", code, "target", target,
			"route", c.Request.Method+" "+c.FullPath())
		return
	}
	scope, ok := service.ScopeFrom(ctx)
	if !ok {
		g.logger().ErrorContext(ctx, "access denial not audited: scope missing", "code", code, "target", target,
			"route", c.Request.Method+" "+c.FullPath())
		return
	}
	g.Denials.Deny(ctx, denial(c, scope, code, op, target))
}

func guardFrom(c *gin.Context) (Guard, bool) {
	v, ok := c.Get(guardKey)
	if !ok {
		return Guard{}, false
	}
	g, ok := v.(Guard)
	return g, ok
}

// abortInternal 记 Error 日志（带路由）后回 500，不向客户端泄露细节。
func abortInternal(c *gin.Context, msg string, err error, attrs ...any) {
	logger := slog.Default()
	if g, ok := guardFrom(c); ok {
		logger = g.logger()
	}
	attrs = append(attrs, "route", c.Request.Method+" "+c.FullPath(), "err", err)
	logger.ErrorContext(c.Request.Context(), msg, attrs...)
	abort(c, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
}
