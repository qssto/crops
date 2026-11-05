package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// 规则下沉影子比对配置（P2 L4，configs/app.yaml 的 rule_offload 段）：
//   - ShadowDiffDays：rule_shadow_diff 保留天数（§6.5：缺省 180，按农场逐项 DELETE … LIMIT 5000 清理）；
//   - PurgeEvery：worker 清理任务周期（缺省 24h）。
//
// 环境变量覆盖：AS_RULE_OFFLOAD_SHADOW_DIFF_DAYS / AS_RULE_OFFLOAD_PURGE_EVERY。
type RuleOffload struct {
	ShadowDiffDays int           `mapstructure:"shadow_diff_days"`
	PurgeEvery     time.Duration `mapstructure:"purge_every"`
}

// 规则下沉配置缺省值。
const (
	DefaultShadowDiffDays        = 180
	DefaultRuleOffloadPurgeEvery = 24 * time.Hour
)

// DefaultRuleOffload 是缺省规则下沉配置（零值装配与测试用）。
func DefaultRuleOffload() RuleOffload {
	return RuleOffload{ShadowDiffDays: DefaultShadowDiffDays, PurgeEvery: DefaultRuleOffloadPurgeEvery}
}

func setRuleOffloadDefaults(v *viper.Viper) {
	v.SetDefault("rule_offload.shadow_diff_days", DefaultShadowDiffDays)
	v.SetDefault("rule_offload.purge_every", DefaultRuleOffloadPurgeEvery)
}

// ErrBadRuleOffload 表示 rule_offload 配置非法。
var ErrBadRuleOffload = errors.New("rule_offload config invalid")

// Validate 要求保留天数与清理周期 > 0。
func (r RuleOffload) Validate() error {
	var errs []error
	if r.ShadowDiffDays <= 0 {
		errs = append(errs, fmt.Errorf("%w: rule_offload.shadow_diff_days must be > 0, got %d", ErrBadRuleOffload, r.ShadowDiffDays))
	}
	if r.PurgeEvery <= 0 {
		errs = append(errs, fmt.Errorf("%w: rule_offload.purge_every must be > 0, got %s", ErrBadRuleOffload, r.PurgeEvery))
	}
	return errors.Join(errs...)
}

// Retention 是保留期时长（ShadowDiffDays 天）。
func (r RuleOffload) Retention() time.Duration {
	return time.Duration(r.ShadowDiffDays) * 24 * time.Hour
}
