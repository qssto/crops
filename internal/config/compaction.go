package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// 长对话压缩缺省（P2 §6.7）：session_idle 30min、keep_recent_turns 4 照抄路线；
// trigger_tokens / summary_max_tokens / max_block_tokens / summary_timeout 为草拟值「待线上数据」
// （无 P0-P1 线上 token 分布与轻量档模型时延）。
const (
	DefaultCompactionTriggerTokens    = 1200
	DefaultCompactionSessionIdle      = 30 * time.Minute
	DefaultCompactionKeepRecentTurns  = 4
	DefaultCompactionSummaryMaxTokens = 300
	DefaultCompactionMaxBlockTokens   = 6000
	DefaultCompactionSummaryTimeout   = 10 * time.Second
)

// Compaction 是 L6 长对话压缩配置；只在 enable_compaction 开时生效。
type Compaction struct {
	// TriggerTokens 是上下文组装估算 token 的触发阈值（严格大于即压缩）。
	TriggerTokens int `mapstructure:"trigger_tokens"`
	// SessionIdle 是 prev_run_id 续接窗口：上一 run 的 updated_at 须在 now - SessionIdle 之后。
	SessionIdle time.Duration `mapstructure:"session_idle"`
	// KeepRecentTurns 是压缩时保留原文的最近轮数。
	KeepRecentTurns int `mapstructure:"keep_recent_turns"`
	// SummaryMaxTokens 是摘要估算 token 上限。
	SummaryMaxTokens int `mapstructure:"summary_max_tokens"`
	// MaxBlockTokens 是整个会话块（含白名单 facts）的估算 token 硬上限，超出时裁剪最早的原文轮次 / 摘要。
	MaxBlockTokens int `mapstructure:"max_block_tokens"`
	// SummaryTimeout 是单次摘要调用超时，超时放弃压缩（COMPACTION_FAILED）。
	SummaryTimeout time.Duration `mapstructure:"summary_timeout"`
}

func setCompactionDefaults(v *viper.Viper) {
	v.SetDefault("compaction.trigger_tokens", DefaultCompactionTriggerTokens)
	v.SetDefault("compaction.session_idle", DefaultCompactionSessionIdle)
	v.SetDefault("compaction.keep_recent_turns", DefaultCompactionKeepRecentTurns)
	v.SetDefault("compaction.summary_max_tokens", DefaultCompactionSummaryMaxTokens)
	v.SetDefault("compaction.max_block_tokens", DefaultCompactionMaxBlockTokens)
	v.SetDefault("compaction.summary_timeout", DefaultCompactionSummaryTimeout)
}

// ErrBadCompaction 表示 compaction 配置非法。
var ErrBadCompaction = errors.New("compaction config invalid")

// Validate 要求各项为正，摘要上限小于触发阈值（否则压缩后估算值不回落，每轮重复触发），
// 硬上限大于触发阈值（否则未到触发就开始裁剪原文）。
func (c Compaction) Validate() error {
	var errs []error
	if c.TriggerTokens <= 0 {
		errs = append(errs, fmt.Errorf("%w: compaction.trigger_tokens must be > 0, got %d", ErrBadCompaction, c.TriggerTokens))
	}
	if c.SessionIdle <= 0 {
		errs = append(errs, fmt.Errorf("%w: compaction.session_idle must be > 0, got %s", ErrBadCompaction, c.SessionIdle))
	}
	if c.KeepRecentTurns <= 0 {
		errs = append(errs, fmt.Errorf("%w: compaction.keep_recent_turns must be > 0, got %d", ErrBadCompaction, c.KeepRecentTurns))
	}
	if c.SummaryMaxTokens <= 0 || c.SummaryMaxTokens >= c.TriggerTokens {
		errs = append(errs, fmt.Errorf("%w: compaction.summary_max_tokens must be in (0, trigger_tokens), got %d",
			ErrBadCompaction, c.SummaryMaxTokens))
	}
	if c.MaxBlockTokens <= c.TriggerTokens {
		errs = append(errs, fmt.Errorf("%w: compaction.max_block_tokens must be > trigger_tokens, got %d",
			ErrBadCompaction, c.MaxBlockTokens))
	}
	if c.SummaryTimeout <= 0 {
		errs = append(errs, fmt.Errorf("%w: compaction.summary_timeout must be > 0, got %s", ErrBadCompaction, c.SummaryTimeout))
	}
	return errors.Join(errs...)
}
