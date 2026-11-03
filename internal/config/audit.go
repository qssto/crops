package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
)

// Retention 是保留期配置（configs/app.yaml 的 retention 段，P1 L7 §6.8 技术实现第 7 条）：
//   - RawObjectDays：raw_object.kind → 保留天数（RawStore.Put 写 expire_at，retention 任务到期删对象）；
//   - TableDays：表名 → 保留天数，只登记不删（audit_log / run_event 不由 retention 任务清理）。
//
// 天数是草拟值「待法务确认」。Load 不补缺省：缺任何 kind 由 app 装配时 RequireKinds 报错、启动失败。
// viper 把 map 键转成小写，取值一律经 RawDays（键转大写，对齐 raw_object.kind）。
type Retention struct {
	RawObjectDays map[string]int `mapstructure:"raw_object_days"`
	TableDays     map[string]int `mapstructure:"table_days"`
}

// RawDays 返回 kind（大写）→ 天数。
func (r Retention) RawDays() map[string]int {
	out := make(map[string]int, len(r.RawObjectDays))
	for k, d := range r.RawObjectDays {
		out[strings.ToUpper(k)] = d
	}
	return out
}

// RequireKinds 校验 kinds 每个都配置了 > 0 的天数，且没有未知 kind（拼写错误不静默）；table_days 天数 > 0。
func (r Retention) RequireKinds(kinds []string) error {
	days := r.RawDays()
	var errs []error
	for _, k := range kinds {
		if d, ok := days[k]; !ok || d <= 0 {
			errs = append(errs, fmt.Errorf("config: retention.raw_object_days.%s must be > 0", k))
		}
	}
	for k := range days {
		if !slices.Contains(kinds, k) {
			errs = append(errs, fmt.Errorf("config: retention.raw_object_days.%s is not a raw_object kind", k))
		}
	}
	for t, d := range r.TableDays {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("config: retention.table_days.%s must be > 0", t))
		}
	}
	return errors.Join(errs...)
}

// Audit 是审计配置（configs/app.yaml 的 audit 段，P1 L7）：
//   - AnchorBucket：每日锚点 bucket（cmd/migrate up 建桶并开 object lock COMPLIANCE）；
//   - AnchorLockDays：锚点对象 COMPLIANCE 保留天数（草拟 365「待法务确认」）；
//   - ExportMaxSpan：审计导出 [from, to) 最大跨度（缺省 744h = 31 天）。
//
// 环境变量覆盖：AS_AUDIT_ANCHOR_BUCKET / AS_AUDIT_ANCHOR_LOCK_DAYS / AS_AUDIT_EXPORT_MAX_SPAN。
type Audit struct {
	AnchorBucket   string        `mapstructure:"anchor_bucket"`
	AnchorLockDays int           `mapstructure:"anchor_lock_days"`
	ExportMaxSpan  time.Duration `mapstructure:"export_max_span"`
}

// 审计配置缺省值。
const (
	DefaultAnchorBucket       = "agri-steward-audit-anchor"
	DefaultAnchorLockDays     = 365 // 待法务确认
	DefaultAuditExportMaxSpan = 31 * 24 * time.Hour
)

// DefaultAudit 是缺省审计配置（评测与测试装配用）。
func DefaultAudit() Audit {
	return Audit{AnchorBucket: DefaultAnchorBucket, AnchorLockDays: DefaultAnchorLockDays,
		ExportMaxSpan: DefaultAuditExportMaxSpan}
}

// setAuditDefaults 注册 audit 段缺省值。
func setAuditDefaults(v *viper.Viper) {
	v.SetDefault("audit.anchor_bucket", DefaultAnchorBucket)
	v.SetDefault("audit.anchor_lock_days", DefaultAnchorLockDays)
	v.SetDefault("audit.export_max_span", DefaultAuditExportMaxSpan)
}

// Validate 校验 bucket 非空、天数与跨度 > 0。
func (a Audit) Validate() error {
	var errs []error
	if a.AnchorBucket == "" {
		errs = append(errs, errors.New("config: audit.anchor_bucket is empty"))
	}
	if a.AnchorLockDays <= 0 {
		errs = append(errs, errors.New("config: audit.anchor_lock_days must be > 0"))
	}
	if a.ExportMaxSpan <= 0 {
		errs = append(errs, errors.New("config: audit.export_max_span must be > 0"))
	}
	return errors.Join(errs...)
}

// ReadOnlyDSN 返回只读会话的 DSN（cmd/auditctl）：连接建立时执行 SET transaction_read_only=1，任何写语句被 MySQL 拒绝。
// 生产建议另配只有 SELECT 权限的账号（AS_MYSQL_USER / AS_MYSQL_PASSWORD 覆盖）。
func (m MySQL) ReadOnlyDSN(db string) (string, error) {
	c, err := mysql.ParseDSN(m.DSN(db, false))
	if err != nil {
		return "", fmt.Errorf("config: parse mysql dsn: %w", err)
	}
	if c.Params == nil {
		c.Params = map[string]string{}
	}
	c.Params["transaction_read_only"] = "1"
	return c.FormatDSN(), nil
}
