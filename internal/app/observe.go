package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1 L7-B 观测装配：业务指标注册表（/metrics）与告警出口（指标 + ALERT.* 审计 + Error 日志）。
// boot 在装配 MCP gateway 之前调 NewObserve（gateway 的 model_actuate 告警与 MCP 指标要用），再经 Deps 传入；
// 评测等未传入的场景由 buildObserve 补缺省（同一套实现，审计写进各自的库）。

// NewObserve 构造指标观测与告警出口（告警出口用同一个观测计 as_alert_total）；auditWriteTimeout 是 ALERT.* 审计短写超时
// （alerts.audit_write_timeout，须在 (0, service.MaxAlertWriteTimeout] 内）。
func NewObserve(db *gorm.DB, c clock.Clock, log *slog.Logger, auditWriteTimeout time.Duration) (*service.MetricsObserver,
	*service.AuditAlertSink, error) {
	obs := service.NewMetricsObserver()
	sink, err := newAlertSink(db, c, log, obs, auditWriteTimeout)
	if err != nil {
		return nil, nil, err
	}
	return obs, sink, nil
}

func newAlertSink(db *gorm.DB, c clock.Clock, log *slog.Logger, obs service.Observer,
	timeout time.Duration) (*service.AuditAlertSink, error) {
	sink, err := service.NewAuditAlertSink(repository.AuditRepo{DB: db}, c, log, obs)
	if err != nil {
		return nil, fmt.Errorf("app: build alert sink: %w", err)
	}
	if err := sink.SetWriteTimeout(timeout); err != nil {
		return nil, fmt.Errorf("app: build alert sink: %w", err)
	}
	return sink, nil
}

// buildObserve 补齐 Deps.Metrics / Deps.Alerts / Deps.DenialCounter / Deps.AlertsConfig 的缺省（须在 buildAccess 之前）；
// AlertsConfig 先补缺省并校验，缺省告警出口的写超时取自它。
func buildObserve(deps *Deps) error {
	if deps.AlertsConfig == (config.Alerts{}) {
		deps.AlertsConfig = config.DefaultAlerts()
	}
	if err := deps.AlertsConfig.Validate(); err != nil {
		return fmt.Errorf("app: build observe: %w", err)
	}
	if deps.Metrics == nil {
		deps.Metrics = service.NewMetricsObserver()
	}
	if deps.Alerts == nil {
		sink, err := newAlertSink(deps.DB, deps.Clock, deps.Logger, deps.Metrics, deps.AlertsConfig.AuditWriteTimeout)
		if err != nil {
			return err
		}
		deps.Alerts = sink
	}
	if deps.DenialCounter == nil {
		deps.DenialCounter = deps.Metrics
	}
	return nil
}

// LoopBacklog 是审批积压 gauge 刷新任务名（只在 worker 跑）。
const LoopBacklog = "approval_backlog"

// buildAlertList 装配告警列表（GET /alerts）。
func (a *App) buildAlertList() error {
	svc, err := service.NewAlertService(repository.AlertRepo{DB: a.DB}, a.Clock)
	if err != nil {
		return fmt.Errorf("app: build alert list: %w", err)
	}
	a.AlertList = svc
	return nil
}

// backlogLoop 构造审批积压 gauge 刷新任务（as_approval_backlog，周期 alerts.backlog_refresh）。
func (a *App) backlogLoop() (Loop, error) {
	r, err := service.NewBacklogRefresher(repository.AlertRepo{DB: a.DB}, a.Observer, a.Logger, a.alerts.BacklogRefresh)
	if err != nil {
		return Loop{}, fmt.Errorf("app: build backlog refresher: %w", err)
	}
	return Loop{Name: LoopBacklog, Run: r.Run}, nil
}

// LoopMetrics 是 /metrics 监听任务名（server 与 worker 各一个，地址见 metrics.*_addr）。
const LoopMetrics = "metrics"

// metricsShutdownTimeout 是 /metrics 监听优雅关闭上限。
const metricsShutdownTimeout = 5 * time.Second

// MetricsLoop 返回在 addr 上只暴露 path（/metrics）的监听任务：不挂业务路由、不经业务鉴权；
// 监听失败只记 Error 日志（指标不可用不拖垮业务进程）。监听期间按 alerts.backlog_refresh 周期预置已登记农场的
// 0 值序列（service.MetricsPreseeder，审查修复 P0）。
func (a *App) MetricsLoop(addr, path string) Loop {
	mux := http.NewServeMux()
	mux.Handle(path, a.MetricsHandler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: metricsShutdownTimeout}
	pre, preErr := service.NewMetricsPreseeder(repository.AlertRepo{DB: a.DB}, a.metrics, a.Logger, a.alerts.BacklogRefresh)
	return Loop{Name: LoopMetrics, Run: func(ctx context.Context) {
		pctx, stopPreseed := context.WithCancel(ctx)
		preDone := make(chan struct{})
		if preErr != nil {
			a.Logger.Error("metrics preseed disabled", "err", preErr)
			close(preDone)
		} else {
			go func() { defer close(preDone); pre.Run(pctx) }()
		}
		defer func() { stopPreseed(); <-preDone }()
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()
		a.Logger.Info("metrics listening", "addr", addr, "path", path)
		select {
		case err := <-errCh:
			a.Logger.Error("metrics listen failed", "addr", addr, "err", err)
			return
		case <-ctx.Done():
		}
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			a.Logger.Warn("metrics shutdown", "err", err)
		}
		if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Warn("metrics listen", "addr", addr, "err", err)
		}
	}}
}

// MetricsHandler 返回 /metrics 处理器（Prometheus 文本格式；不经业务鉴权，只挂在 metrics.*_addr 监听上）。
func (a *App) MetricsHandler() http.Handler { return a.metrics.Registry().Handler(a.Logger) }

// MetricsRegistry 返回指标注册表（测试与规则校验用）。
func (a *App) MetricsRegistry() *service.MetricRegistry { return a.metrics.Registry() }
