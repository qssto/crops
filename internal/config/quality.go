package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// 可信度日报缺省（P2 §6.4 L3 技术实现 2-4）：缺省不启用（worker 不起 quality-daily，行为不变）。
const (
	DefaultQualityInterval          = time.Hour
	DefaultQualitySamplePerCategory = 5
	DefaultQualitySampleSalt        = "p2l3"
	DefaultQualityJudgeTier         = "standard"
	DefaultQualityJudgeQPM          = 6
	DefaultQualityJudgesDir         = "eval/judges"
	DefaultQualityBaselineName      = "baseline_p1.r2"
	DefaultQualityReportTimezone    = "Asia/Shanghai"
)

// Quality 是 configs/app.yaml 的 quality 段：线上 rollup、online judge 抽样与日报。
type Quality struct {
	// Enabled 为 false 时 worker 不登记 quality-daily 任务。
	Enabled bool `mapstructure:"enabled"`
	// Interval 是检查周期；每个周期只在「前一日」变化后真正执行（补跑幂等）。
	Interval time.Duration `mapstructure:"interval"`
	// SamplePerCategory 是每农场每 category 每日抽样 judge 的卡数（ORDER BY CRC32(CONCAT(id, salt)) LIMIT n）。
	SamplePerCategory int `mapstructure:"sample_per_category"`
	// SampleSalt 是抽样盐：同盐同数据抽到同一批卡（可复现）。
	SampleSalt string `mapstructure:"sample_salt"`
	// JudgeTier 是 judge 用的模型档位：先取生产 rl:model:{tier} 令牌，再受 rl:judge:{tier} 约束。
	JudgeTier string `mapstructure:"judge_tier"`
	// JudgeQPM 是 rl:judge:{tier} 每分钟上限。
	JudgeQPM int `mapstructure:"judge_qpm"`
	// JudgesDir 是 judge 提示词与录制目录（相对 --repo；L2 的 eval/judges）。
	JudgesDir string `mapstructure:"judges_dir"`
	// BaselineName / BaselineExperimentID 是日报差值的基线；ID 为 0 表示基线尚未冻结，差值列显示「—」。
	BaselineName         string `mapstructure:"baseline_name"`
	BaselineExperimentID uint64 `mapstructure:"baseline_experiment_id"`
	// ReportTimezone 决定日报文件名 {yyyymmdd} 与平台行的业务日。
	ReportTimezone string `mapstructure:"report_timezone"`
}

func setQualityDefaults(v *viper.Viper) {
	v.SetDefault("quality.enabled", false)
	v.SetDefault("quality.interval", DefaultQualityInterval)
	v.SetDefault("quality.sample_per_category", DefaultQualitySamplePerCategory)
	v.SetDefault("quality.sample_salt", DefaultQualitySampleSalt)
	v.SetDefault("quality.judge_tier", DefaultQualityJudgeTier)
	v.SetDefault("quality.judge_qpm", DefaultQualityJudgeQPM)
	v.SetDefault("quality.judges_dir", DefaultQualityJudgesDir)
	v.SetDefault("quality.baseline_name", DefaultQualityBaselineName)
	v.SetDefault("quality.report_timezone", DefaultQualityReportTimezone)
}

// ErrBadQuality 表示 quality 配置非法。
var ErrBadQuality = errors.New("quality config invalid")

// Validate 要求周期、抽样数、judge QPM 为正，档位 / 盐 / judge 目录非空，时区可加载。未启用也校验（失败关闭）。
func (q Quality) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrBadQuality}, args...)...))
	}
	if q.Interval <= 0 {
		bad("quality.interval must be > 0, got %v", q.Interval)
	}
	if q.SamplePerCategory <= 0 {
		bad("quality.sample_per_category must be > 0, got %d", q.SamplePerCategory)
	}
	if q.JudgeQPM <= 0 {
		bad("quality.judge_qpm must be > 0, got %d", q.JudgeQPM)
	}
	for _, f := range [][2]string{{"sample_salt", q.SampleSalt}, {"judge_tier", q.JudgeTier}, {"judges_dir", q.JudgesDir}} {
		if f[1] == "" {
			bad("quality.%s is required", f[0])
		}
	}
	if q.ReportTimezone == "" {
		bad("quality.report_timezone is required")
	} else if _, err := time.LoadLocation(q.ReportTimezone); err != nil {
		bad("quality.report_timezone %q: %v", q.ReportTimezone, err)
	}
	return errors.Join(errs...)
}
