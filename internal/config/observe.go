package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Metrics 是 /metrics 暴露配置（configs/app.yaml 的 metrics 段，P1 L7-B）：server 与 worker 各自在独立监听上暴露，
// 不经业务鉴权，只监听配置的地址（缺省 127.0.0.1，生产按部署网段改）。
// 环境变量覆盖：AS_METRICS_SERVER_ADDR、AS_METRICS_WORKER_ADDR、AS_METRICS_PATH。
type Metrics struct {
	ServerAddr string `mapstructure:"server_addr"`
	WorkerAddr string `mapstructure:"worker_addr"`
	Path       string `mapstructure:"path"`
}

// Alerts 是告警扫描配置（configs/app.yaml 的 alerts 段，P1 L7-B）：
// NoCitationWindow 是无引用建议扫描的回看窗口；BacklogRefresh 是审批积压 gauge 的刷新周期；
// AuditWriteTimeout 是告警出口 ALERT.* 审计独立短写的超时（审查修复：Fire 可能在锁内同步调用，上限 500ms）。
// 环境变量覆盖：AS_ALERTS_NO_CITATION_WINDOW、AS_ALERTS_BACKLOG_REFRESH、AS_ALERTS_AUDIT_WRITE_TIMEOUT。
type Alerts struct {
	NoCitationWindow  time.Duration `mapstructure:"no_citation_window"`
	BacklogRefresh    time.Duration `mapstructure:"backlog_refresh"`
	AuditWriteTimeout time.Duration `mapstructure:"audit_write_timeout"`
}

// 观测配置缺省值。MaxAlertsAuditWriteTimeout 与 service.MaxAlertWriteTimeout 同值（config 不依赖 service，测试校验一致）。
const (
	DefaultMetricsServerAddr       = "127.0.0.1:9101"
	DefaultMetricsWorkerAddr       = "127.0.0.1:9102"
	DefaultMetricsPath             = "/metrics"
	DefaultAlertsNoCitationWindow  = 2 * time.Minute
	DefaultAlertsBacklogRefresh    = 30 * time.Second
	DefaultAlertsAuditWriteTimeout = 500 * time.Millisecond
	MaxAlertsAuditWriteTimeout     = 500 * time.Millisecond
)

// DefaultMetrics 是缺省 metrics 配置。
func DefaultMetrics() Metrics {
	return Metrics{ServerAddr: DefaultMetricsServerAddr, WorkerAddr: DefaultMetricsWorkerAddr, Path: DefaultMetricsPath}
}

// DefaultAlerts 是缺省 alerts 配置（评测与测试装配用）。
func DefaultAlerts() Alerts {
	return Alerts{NoCitationWindow: DefaultAlertsNoCitationWindow, BacklogRefresh: DefaultAlertsBacklogRefresh,
		AuditWriteTimeout: DefaultAlertsAuditWriteTimeout}
}

// setObserveDefaults 注册 metrics / alerts 段缺省值。
func setObserveDefaults(v *viper.Viper) {
	v.SetDefault("metrics.server_addr", DefaultMetricsServerAddr)
	v.SetDefault("metrics.worker_addr", DefaultMetricsWorkerAddr)
	v.SetDefault("metrics.path", DefaultMetricsPath)
	v.SetDefault("alerts.no_citation_window", DefaultAlertsNoCitationWindow)
	v.SetDefault("alerts.backlog_refresh", DefaultAlertsBacklogRefresh)
	v.SetDefault("alerts.audit_write_timeout", DefaultAlertsAuditWriteTimeout)
}

// Validate 校验两个监听地址为 host:port、path 以 / 开头。
func (m Metrics) Validate() error {
	var errs []error
	for _, a := range []struct{ name, addr string }{{"server_addr", m.ServerAddr}, {"worker_addr", m.WorkerAddr}} {
		if _, _, err := net.SplitHostPort(a.addr); err != nil {
			errs = append(errs, fmt.Errorf("config: metrics.%s %q: %w", a.name, a.addr, err))
		}
	}
	if !strings.HasPrefix(m.Path, "/") {
		errs = append(errs, fmt.Errorf("config: metrics.path %q must start with /", m.Path))
	}
	return errors.Join(errs...)
}

// Validate 校验 no_citation_window > 0、backlog_refresh > 0、audit_write_timeout ∈ (0, 500ms]。
func (a Alerts) Validate() error {
	var errs []error
	if a.NoCitationWindow <= 0 {
		errs = append(errs, errors.New("config: alerts.no_citation_window must be > 0"))
	}
	if a.BacklogRefresh <= 0 {
		errs = append(errs, errors.New("config: alerts.backlog_refresh must be > 0"))
	}
	if a.AuditWriteTimeout <= 0 || a.AuditWriteTimeout > MaxAlertsAuditWriteTimeout {
		errs = append(errs, fmt.Errorf("config: alerts.audit_write_timeout %v not in (0, %v]",
			a.AuditWriteTimeout, MaxAlertsAuditWriteTimeout))
	}
	return errors.Join(errs...)
}
