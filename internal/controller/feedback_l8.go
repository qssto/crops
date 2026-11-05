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

// P2 L8 采纳 / 差评（§6.9 技术实现 ① 1，权限 advice.feedback 由 routeOps 判定；请求体见 contracts/advice_feedback_request.schema.json）。

// FeedbackSubmitter 是反馈写入能力，service.FeedbackService 实现。
type FeedbackSubmitter interface {
	Submit(ctx context.Context, scope model.Scope, in service.FeedbackInput) (service.FeedbackResult, error)
}

// FeedbackRoutes 挂载 POST /advices/:advice_id/feedback。
func FeedbackRoutes(s FeedbackSubmitter) Mount {
	return func(farms *gin.RouterGroup) {
		farms.POST("/advices/:advice_id/feedback", FeedbackHandler(s))
	}
}

// FeedbackHandler 是 POST …/advices/:advice_id/feedback：201 首次写入，200 同 request_id 重放（返回首次结果）。
// 入参非法 → 400；建议卡不在本农场（不存在或属他场）→ 404 并记 RESOURCE_NOT_IN_SCOPE；request_id 被他场用过 → 409。
func FeedbackHandler(s FeedbackSubmitter) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		adviceID, err := strconv.ParseUint(c.Param("advice_id"), 10, 64)
		if err != nil || adviceID == 0 {
			abort(c, http.StatusBadRequest, "bad advice_id")
			return
		}
		var in service.FeedbackInput
		if err := c.ShouldBindJSON(&in); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		in.AdviceID = adviceID
		res, err := s.Submit(c.Request.Context(), scope, in)
		switch {
		case err == nil:
		case errors.Is(err, service.ErrFeedbackInvalid):
			abort(c, http.StatusBadRequest, service.ErrFeedbackInvalid.Error())
			return
		case errors.Is(err, model.ErrNotFound):
			recordDenial(c, service.DenyResource, service.OpAdviceFeedback, "advice:"+strconv.FormatUint(adviceID, 10))
			abort(c, http.StatusNotFound, http.StatusText(http.StatusNotFound))
			return
		case errors.Is(err, model.ErrFeedbackRequestConflict):
			abort(c, http.StatusConflict, "REQUEST_ID_CONFLICT")
			return
		default:
			abortErr(c, err)
			return
		}
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		c.JSON(status, res)
	}
}
