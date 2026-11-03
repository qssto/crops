package app

import (
	"testing"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/config"
	"agri-steward/internal/service"
)

// 告警审计写超时：config 上限与 service 上限一致（config 不依赖 service，这里对表）；
// NewObserve / buildObserve 拒绝超出 (0, 500ms] 的取值（审查修复 P2）。
func TestObserve_AuditWriteTimeout(t *testing.T) {
	if config.MaxAlertsAuditWriteTimeout != service.MaxAlertWriteTimeout ||
		config.DefaultAlertsAuditWriteTimeout != service.DefaultAlertWriteTimeout {
		t.Fatalf("config/service write timeout drift: config max=%v default=%v, service max=%v default=%v",
			config.MaxAlertsAuditWriteTimeout, config.DefaultAlertsAuditWriteTimeout,
			service.MaxAlertWriteTimeout, service.DefaultAlertWriteTimeout)
	}
	tests := []struct {
		name    string
		d       time.Duration
		wantErr bool
	}{
		{name: "default", d: config.DefaultAlertsAuditWriteTimeout},
		{name: "short", d: 100 * time.Millisecond},
		{name: "zero rejected", d: 0, wantErr: true},
		{name: "over max rejected", d: 2 * time.Second, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := NewObserve(nil, clock.System{}, nil, tt.d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewObserve(%v) err = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
			ac := config.DefaultAlerts()
			ac.AuditWriteTimeout = tt.d
			deps := Deps{Clock: clock.System{}, AlertsConfig: ac}
			err = buildObserve(&deps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildObserve(%v) err = %v, wantErr %v", tt.d, err, tt.wantErr)
			}
			if !tt.wantErr && (deps.Alerts == nil || deps.Metrics == nil || deps.DenialCounter == nil) {
				t.Fatalf("buildObserve left defaults unset: %+v", deps)
			}
		})
	}
}
