package config

import (
	"testing"
	"time"
)

// alerts.audit_write_timeout：缺省 500ms，环境变量可覆盖，超出 (0, 500ms] 拒绝（审查修复 P2：告警审计短写在锁内同步调用）。
func TestAlerts_AuditWriteTimeout(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", want: DefaultAlertsAuditWriteTimeout},
		{name: "env override", env: "200ms", want: 200 * time.Millisecond},
		{name: "env at max", env: "500ms", want: MaxAlertsAuditWriteTimeout},
		{name: "over max rejected", env: "2s", wantErr: true},
		{name: "zero rejected", env: "0s", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("AS_ALERTS_AUDIT_WRITE_TIMEOUT", tt.env)
			}
			c, err := Load("../../configs/app.yaml", "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if c.Alerts.AuditWriteTimeout != tt.want {
				t.Fatalf("audit_write_timeout = %v, want %v", c.Alerts.AuditWriteTimeout, tt.want)
			}
		})
	}
	if err := DefaultAlerts().Validate(); err != nil {
		t.Fatalf("DefaultAlerts invalid: %v", err)
	}
}
