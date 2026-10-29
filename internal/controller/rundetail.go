package controller

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// RunDetailer 是 run 详情能力，service.RunDetailService 实现。
type RunDetailer interface {
	Get(ctx context.Context, scope model.Scope, runID uint64) (service.RunDetail, error)
}

// MetricsReader 是看板指标能力，service.MetricsService 实现。
type MetricsReader interface {
	Daily(ctx context.Context, scope model.Scope, from, to time.Time) (service.MetricsReport, error)
}

// RunRoutes 挂载 run 详情与指标（P0 L7 功能任务 4、5）。
func RunRoutes(d RunDetailer, m MetricsReader) Mount {
	return func(farms *gin.RouterGroup) {
		farms.GET("/runs/:run_id", RunDetailHandler(d))
		farms.GET("/metrics", MetricsHandler(m))
	}
}

// RunDetailHandler 是 GET /api/v1/farms/:farm_id/runs/:run_id：完整链路（JSON 的 run / card 两键兼容 L4）。
func RunDetailHandler(svc RunDetailer) gin.HandlerFunc {
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
		detail, err := svc.Get(c.Request.Context(), scope, runID)
		if err != nil {
			abortErr(c, err)
			return
		}
		c.JSON(http.StatusOK, detail)
	}
}

// MetricsHandler 是 GET /api/v1/farms/:farm_id/metrics?from=YYYY-MM-DD&to=YYYY-MM-DD（UTC 自然日，含首尾；缺省为当天）。
func MetricsHandler(svc MetricsReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := service.ScopeFrom(c.Request.Context())
		if !ok {
			abort(c, http.StatusUnauthorized, "unauthenticated")
			return
		}
		from, err := parseDay(c.Query("from"))
		if err != nil {
			abort(c, http.StatusBadRequest, "bad from")
			return
		}
		to, err := parseDay(c.Query("to"))
		if err != nil {
			abort(c, http.StatusBadRequest, "bad to")
			return
		}
		rep, err := svc.Daily(c.Request.Context(), scope, from, to)
		if err != nil {
			abortErr(c, err)
			return
		}
		c.JSON(http.StatusOK, rep)
	}
}

// parseDay 解析 YYYY-MM-DD（UTC）；空串返回零值（由 service 取当天）。
func parseDay(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation(service.DateLayout, s, time.UTC)
}

// 两个 amis 页面 JSON 的文件名：看板（L7）与管家页（L8，对话 / 待审批 / run 详情）。
const (
	DashboardFile = "dashboard.json"
	StewardFile   = "steward.json"
)

// amisShell 是承载 amis 页面的最小 HTML（amis SDK 走 CDN，页面结构全部在 /dashboard/{file}）。
// title 与 file 只取本包常量，不拼接请求输入。
func amisShell(title, file string) []byte {
	return []byte(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + title + `</title>
<link rel="stylesheet" href="https://unpkg.com/amis@6.13.0/sdk/sdk.css">
<link rel="stylesheet" href="https://unpkg.com/amis@6.13.0/sdk/helper.css">
<script src="https://unpkg.com/amis@6.13.0/sdk/sdk.js"></script>
<style>html,body{margin:0;background:#fff}</style></head>
<body><div id="root"></div>
<script>
fetch('/dashboard/` + file + `').then(function (r) { return r.json(); })
  .then(function (schema) { amisRequire('amis/embed').embed('#root', schema); });
</script></body></html>`)
}

// MountDashboard 在 r 上挂 GET /dashboard 与 GET /steward（HTML 外壳）以及 GET /dashboard/:file（dir 下的 .json 页面）。
// 启动时不检查文件；对应 JSON 缺失时外壳返回 404。页面只含结构，数据经带 Bearer 的 /api/v1 接口读取。
func MountDashboard(r *gin.Engine, dir string) {
	exists := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular()
	}
	shell := func(title, file string) gin.HandlerFunc {
		html := amisShell(title, file)
		return func(c *gin.Context) {
			if !exists(file) {
				abort(c, http.StatusNotFound, strings.TrimSuffix(file, ".json")+" not installed")
				return
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", html)
		}
	}
	r.GET("/dashboard", shell("agri-steward 看板", DashboardFile))
	r.GET("/steward", shell("agri-steward 管家", StewardFile))
	r.GET("/dashboard/:file", func(c *gin.Context) {
		name := c.Param("file")
		// 只服务目录下一层的 .json 文件（:file 不含 /；再排除 .. 与隐藏文件）。
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") || !exists(name) {
			abort(c, http.StatusNotFound, http.StatusText(http.StatusNotFound))
			return
		}
		c.FileFromFS(name, http.Dir(dir))
	})
}
