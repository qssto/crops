package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// P2-L2b 专家评审（平台路由，review.read / review.submit 只授 kb_expert；不做 amis 页面）：
//   GET  /api/v1/admin/review-tasks?task_type=&status=&page=&page_size=
//   GET  /api/v1/admin/review-tasks/:id
//   POST /api/v1/admin/review-tasks/:id/submit
// 评测库连接关闭时路由照挂、鉴权照做，之后回 503 EVAL_REVIEW_DISABLED（不用 404，免得与「任务不存在」混淆）。

// ReviewAdmin 是评审能力，service.ReviewService 实现。
type ReviewAdmin interface {
	List(ctx context.Context, actor model.Scope, q service.ReviewListQuery) (model.ReviewTaskPage, error)
	Get(ctx context.Context, actor model.Scope, id uint64) (model.ReviewTaskView, error)
	Submit(ctx context.Context, actor model.Scope, id uint64, in model.ReviewSubmit) (model.ReviewSubmitResult, error)
}

// maxReviewBody 是提交体上限（expect 上限加上其余字段的余量）。
const maxReviewBody = model.MaxReviewExpectBytes + 4<<10

// MountReviewAdmin 挂载评审路由（平台 Scope；读与提交各自 RequireOps）。
func MountReviewAdmin(r *gin.Engine, g Guard, s ReviewAdmin) {
	read := r.Group(AdminPrefix, RequestID(), PlatformAuth(g), RequireOps(g, service.OpReviewRead))
	read.GET("/review-tasks", ReviewListHandler(s))
	read.GET("/review-tasks/:id", ReviewGetHandler(s))
	submit := r.Group(AdminPrefix, RequestID(), PlatformAuth(g), RequireOps(g, service.OpReviewSubmit))
	submit.POST("/review-tasks/:id/submit", ReviewSubmitHandler(s))
}

// ReviewAdminRoutes 是 MountReviewAdmin 的 EngineMount 形式（app 装配用，app 不 import gin）。
func ReviewAdminRoutes(g Guard, s ReviewAdmin) EngineMount {
	return func(r *gin.Engine) { MountReviewAdmin(r, g, s) }
}

// ReviewListHandler 是列表：200 {items,total,page,page_size}；查询参数非法 → 400。
func ReviewListHandler(s ReviewAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		q := service.ReviewListQuery{TaskType: c.Query("task_type"), Status: c.Query("status")}
		var err error
		if q.Page, err = queryInt(c, "page"); err != nil {
			abort(c, http.StatusBadRequest, "bad page")
			return
		}
		if q.PageSize, err = queryInt(c, "page_size"); err != nil {
			abort(c, http.StatusBadRequest, "bad page_size")
			return
		}
		page, err := s.List(c.Request.Context(), scope, q)
		if err != nil {
			reviewAbort(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

// ReviewGetHandler 是详情：holdout 任务只回 ID 与状态类字段；不存在 → 404。
func ReviewGetHandler(s ReviewAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, id, ok := reviewTarget(c)
		if !ok {
			return
		}
		v, err := s.Get(c.Request.Context(), scope, id)
		if err != nil {
			reviewAbort(c, err)
			return
		}
		c.JSON(http.StatusOK, v)
	}
}

// ReviewSubmitHandler 是提交：200 {task,replayed}；提交体非法 / 含 PII → 400，状态冲突 → 409 REVIEW_CONFLICT。
func ReviewSubmitHandler(s ReviewAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, id, ok := reviewTarget(c)
		if !ok {
			return
		}
		in, err := decodeReviewSubmit(c)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		res, err := s.Submit(c.Request.Context(), scope, id, in)
		if err != nil {
			reviewAbort(c, err)
			return
		}
		c.JSON(http.StatusOK, res)
	}
}

// decodeReviewSubmit 严格解码提交体（未知字段、尾随内容、超长都拒绝，与契约 additionalProperties:false 一致）。
func decodeReviewSubmit(c *gin.Context) (model.ReviewSubmit, error) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxReviewBody))
	if err != nil {
		return model.ReviewSubmit{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var in model.ReviewSubmit
	if err := dec.Decode(&in); err != nil {
		return model.ReviewSubmit{}, err
	}
	if dec.More() {
		return model.ReviewSubmit{}, errors.New("trailing data")
	}
	return in, nil
}

// reviewTarget 取 Scope 与路径 :id；失败时已写响应。
func reviewTarget(c *gin.Context) (model.Scope, uint64, bool) {
	scope, ok := service.ScopeFrom(c.Request.Context())
	if !ok {
		abort(c, http.StatusUnauthorized, "unauthenticated")
		return model.Scope{}, 0, false
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		abort(c, http.StatusBadRequest, "bad id")
		return model.Scope{}, 0, false
	}
	return scope, id, true
}

// queryInt 读可选整数查询参数（缺省 0，交给 service 补缺省与校验）。
func queryInt(c *gin.Context, name string) (int, error) {
	v := c.Query(name)
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// reviewAbort 把评审错误映射为状态码；错误码原样回给客户端。
func reviewAbort(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrReviewUnavailable):
		abort(c, http.StatusServiceUnavailable, service.ErrReviewUnavailable.Error())
	case errors.Is(err, service.ErrRoleInsufficient):
		abort(c, http.StatusForbidden, service.ErrRoleInsufficient.Error())
	case errors.Is(err, service.ErrReviewConflict):
		abort(c, http.StatusConflict, service.ErrReviewConflict.Error())
	case errors.Is(err, service.ErrReviewPII):
		abort(c, http.StatusBadRequest, service.ErrReviewPII.Error())
	case errors.Is(err, model.ErrReviewSubmit):
		abort(c, http.StatusBadRequest, "REVIEW_SUBMIT_INVALID")
	default:
		abortErr(c, err)
	}
}
