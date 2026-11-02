package config

import (
	"errors"
	"time"

	"github.com/spf13/viper"
)

// Approval 是审批体系配置（configs/app.yaml 的 approval 段，P1 L5）：
// RejectionExportMaxSpan 是驳回导出 GET /rejections 的 [from, to] 最大跨度，超限 400。
// 环境变量覆盖：AS_APPROVAL_REJECTION_EXPORT_MAX_SPAN。
type Approval struct {
	RejectionExportMaxSpan time.Duration `mapstructure:"rejection_export_max_span"`
}

// DefaultRejectionExportMaxSpan 是驳回导出跨度缺省值（31 天）。
const DefaultRejectionExportMaxSpan = 31 * 24 * time.Hour

// DefaultApproval 是缺省审批配置（评测与测试装配用）。
func DefaultApproval() Approval {
	return Approval{RejectionExportMaxSpan: DefaultRejectionExportMaxSpan}
}

// setApprovalDefaults 注册 approval 段缺省值。
func setApprovalDefaults(v *viper.Viper) {
	v.SetDefault("approval.rejection_export_max_span", DefaultRejectionExportMaxSpan)
}

// Validate 校验 rejection_export_max_span > 0。
func (a Approval) Validate() error {
	if a.RejectionExportMaxSpan <= 0 {
		return errors.New("config: approval.rejection_export_max_span must be > 0")
	}
	return nil
}
