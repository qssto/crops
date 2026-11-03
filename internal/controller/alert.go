package controller

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// P1 L7-B 告警列表（规则 annotation 下钻入口，权限复用看板读 advice.read）。

// AlertLister 是告警列表能力，service.AlertService 实现。
type AlertLister interface {
	List(ctx context.Context, scope model.Scope, q service.AlertQuery) (service.AlertList, error)
}

// AlertRoutes 挂载 GET /alerts。
func AlertRoutes(s AlertLister) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/alerts", AlertListHandler(s))
	}
}

// AlertListHandler 是 GET /api/v1/farms/:farm_id/alerts?name=&from=&to=&limit=&cursor=（from / to 为 RFC3339，可缺省：
// to 缺省为当前、from 缺省为 to-24h；limit 缺省 50、上限 200；cursor 为上一页的 next_cursor，翻页时其余参数不变）。
// 时间倒序；参数非法 / 告警名未登记 / 跨度超 31 天 / 游标非法 → 400。
func AlertListHandler(s AlertLister) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		q := service.AlertQuery{Name: model.AlertName(c.Query("name")), Cursor: c.Query("cursor")}
		var err error
		if q.From, err = optionalRFC3339(c.Query("from")); err != nil {
			abort(c, http.StatusBadRequest, "bad from (RFC3339)")
			return
		}
		if q.To, err = optionalRFC3339(c.Query("to")); err != nil {
			abort(c, http.StatusBadRequest, "bad to (RFC3339)")
			return
		}
		if v := c.Query("limit"); v != "" {
			if q.Limit, err = strconv.Atoi(v); err != nil || q.Limit <= 0 {
				abort(c, http.StatusBadRequest, "bad limit")
				return
			}
		}
		list, err := s.List(c.Request.Context(), scope, q)
		if err != nil {
			abortErr(c, err)
			return
		}
		c.JSON(http.StatusOK, list)
	}
}

func optionalRFC3339(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
