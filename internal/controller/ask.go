package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// HeaderRequestID 是请求追踪头，原样回写到响应；缺省时生成。
const HeaderRequestID = "X-Request-Id"

const requestIDBytes = 16

// requestIDPattern 限定外部传入的 request id，避免日志注入。
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Asker 是 ask 入口需要的能力，service.AskService 实现。GET /runs/{run_id} 自 L7 由 RunRoutes 提供（RunDetail ⊇ RunView）。
type Asker interface {
	Ask(ctx context.Context, scope model.Scope, in service.AskInput) (uint64, error)
}

// askRequest 是 POST /ask 请求体；farm_id 只取路径。
type askRequest struct {
	FieldID uint64 `json:"field_id"`
	Text    string `json:"text"`
}

// askResponse 是 202 响应体。
type askResponse struct {
	RunID uint64 `json:"run_id"`
}

// RequestID 透传或生成 X-Request-Id：放入 ctx 并回写响应头；格式非法 → 400。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(HeaderRequestID)
		if id == "" {
			buf := make([]byte, requestIDBytes)
			if _, err := rand.Read(buf); err != nil {
				abort(c, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
				return
			}
			id = hex.EncodeToString(buf)
		} else if !requestIDPattern.MatchString(id) {
			abort(c, http.StatusBadRequest, "bad "+HeaderRequestID)
			return
		}
		c.Header(HeaderRequestID, id)
		c.Request = c.Request.WithContext(service.WithRequestID(c.Request.Context(), id))
		c.Next()
	}
}

// AskHandler 是 POST /api/v1/farms/:farm_id/ask：校验后立即返回 202 {run_id}，主链路异步执行。
func AskHandler(svc Asker) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var req askRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abort(c, http.StatusBadRequest, "bad body")
			return
		}
		runID, err := svc.Ask(c.Request.Context(), scope, service.AskInput{FieldID: req.FieldID, Text: req.Text})
		if err != nil {
			abortErr(c, err)
			return
		}
		c.JSON(http.StatusAccepted, askResponse{RunID: runID})
	}
}

// abortErr 把 service 错误映射为 HTTP 状态；未知错误只回 500，不泄露细节。
func abortErr(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, service.ErrBadInput):
		status = http.StatusBadRequest
	case errors.Is(err, service.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, model.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, service.ErrRateLimited):
		status = http.StatusTooManyRequests
	}
	abort(c, status, http.StatusText(status))
}
