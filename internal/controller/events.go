package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// EventsPath 是 SSE 事件流的完整路由；FarmAuth 只在此路由接受 ?access_token=（EventSource 不能带 Authorization 头）。
const EventsPath = "/api/v1/farms/:farm_id/runs/:run_id/events"

// lastEventIDHeader 是 EventSource 重连时带的游标头。
const lastEventIDHeader = "Last-Event-ID"

// EventStreamer 是 run 事件流能力，service.EventService 实现。
type EventStreamer interface {
	Stream(ctx context.Context, scope model.Scope, runID uint64, lastSeq int64, sink service.StreamSink) error
}

// EventRoutes 挂载 GET /runs/:run_id/events（P0 §6.9 流程 ①2，L8 冻结契约）。
func EventRoutes(s EventStreamer) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/runs/:run_id/events", EventsHandler(s))
	}
}

// EventsHandler 只做参数校验与转发：游标取 Last-Event-ID 头，缺省取 ?last_event_id=，都没有从 0 开始。
// 首帧写出前的错误按 JSON 错误码返回；之后的错误只能断开连接（客户端按 Last-Event-ID 重连）。
func EventsHandler(s EventStreamer) gin.HandlerFunc {
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
		last, err := lastEventID(c)
		if err != nil {
			abort(c, http.StatusBadRequest, "bad last_event_id")
			return
		}
		sink := &sseSink{c: c}
		err = s.Stream(c.Request.Context(), scope, runID, last, sink)
		switch {
		case err == nil:
		case !sink.started:
			abortErr(c, err)
		case !errors.Is(err, context.Canceled):
			// 已开始推流，只能断开；错误挂到 gin 错误链供日志中间件读取
			c.Error(err) //nolint:errcheck // 返回值即入参包装，无新错误
		}
	}
}

// lastEventID 解析游标；空即 0。
func lastEventID(c *gin.Context) (int64, error) {
	v := c.GetHeader(lastEventIDHeader)
	if v == "" {
		v = c.Query("last_event_id")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("last_event_id %q: %w", v, service.ErrBadInput)
	}
	return n, nil
}

// sseSink 把 service.StreamEvent 写成 text/event-stream 帧并逐帧 flush。
type sseSink struct {
	c       *gin.Context
	started bool
}

func (s *sseSink) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.c.Status(http.StatusOK)
}

func (s *sseSink) write(frame string) error {
	s.start()
	if _, err := s.c.Writer.WriteString(frame); err != nil {
		return fmt.Errorf("controller: sse write: %w", err)
	}
	s.c.Writer.Flush()
	return nil
}

// Event 实现 service.StreamSink：id: {seq}\nevent: {event_type}\ndata: {json}\n\n。
func (s *sseSink) Event(e service.StreamEvent) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("controller: sse encode seq %d: %w", e.Seq, err)
	}
	return s.write(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.EventType, data))
}

// Ping 实现 service.StreamSink（SSE 注释行保活）。
func (s *sseSink) Ping() error { return s.write(": ping\n\n") }

// End 实现 service.StreamSink：run 结束后发 event: end，随后 handler 返回、连接关闭。
func (s *sseSink) End() error { return s.write("event: end\ndata: {}\n\n") }
