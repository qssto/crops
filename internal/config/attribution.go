package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/viper"
)

// 归因周报与线上回流配置（P2 L8，configs/app.yaml 的 attribution 段，§6.9 技术实现 2 / 3）：
//   - Enabled：worker 是否起 attribution-weekly（缺省 false）；
//   - Timezone：周界（ISO 周一 00:00）所在时区；TopN：周报列出的问题数；
//   - Interval：worker 检查周期（「上周周报已出」看 eval 桶 done 标记，周一首个周期出上周周报）；
//   - KBLoadGapsKey：eval 桶内 L5 kb_load_gaps.json 的对象键（有就附一节，无就写「无」）；
//   - Backflow：backflow-collect（缺省 false）。
//
// 环境变量覆盖：AS_ATTRIBUTION_ENABLED / AS_ATTRIBUTION_TOP_N / AS_ATTRIBUTION_BACKFLOW_ENABLED 等。
type Attribution struct {
	Enabled       bool          `mapstructure:"enabled"`
	Timezone      string        `mapstructure:"timezone"`
	TopN          int           `mapstructure:"top_n"`
	Interval      time.Duration `mapstructure:"interval"`
	KBLoadGapsKey string        `mapstructure:"kb_load_gaps_key"`
	Backflow      Backflow      `mapstructure:"backflow"`
}

// Backflow 是 backflow-collect（每周选样 → 脱敏 → 写草稿）配置：
//   - DraftsDir：草稿目录（eval/backflow/drafts，只提交 README / .gitkeep；相对路径相对仓库根）；
//   - KBMissMinHits：同一问题类型 KB_MISS 拒答一周达到该次数才入选（「高频」阈值，待线上数据）；
//   - MaxRuns：单农场每周最多选样 run 数；
//   - ServiceUserID：读原文的服务账号（RAW_READ 审计记 actor）。
type Backflow struct {
	Enabled       bool   `mapstructure:"enabled"`
	DraftsDir     string `mapstructure:"drafts_dir"`
	KBMissMinHits int    `mapstructure:"kb_miss_min_hits"`
	MaxRuns       int    `mapstructure:"max_runs"`
	ServiceUserID uint64 `mapstructure:"service_user_id"`
}

// 归因 / 回流配置缺省值。
const (
	DefaultAttributionTimezone = "Asia/Shanghai"
	DefaultAttributionTopN     = 20
	DefaultAttributionInterval = time.Hour
	DefaultKBLoadGapsKey       = "nightly/kb_load_gaps.json"
	DefaultBackflowDraftsDir   = "eval/backflow/drafts"
	DefaultBackflowKBMissMin   = 3 // 待线上数据
	DefaultBackflowMaxRuns     = 50
	// DefaultBackflowServiceUser 是读原文的服务账号缺省 user_id：取值避开种子用户（configs/seed/fields.yaml 的 1001…、
	// 2001…、平台管理员 9001），configs/app.yaml 的 service_user_id 与此一致；部署须按实际服务账号覆盖。
	DefaultBackflowServiceUser  = 9900
	attributionKBLoadGapsKeyMax = 512
)

// DefaultAttribution 是缺省配置（零值装配与测试用；两项任务均不启用）。
func DefaultAttribution() Attribution {
	return Attribution{Timezone: DefaultAttributionTimezone, TopN: DefaultAttributionTopN, Interval: DefaultAttributionInterval,
		KBLoadGapsKey: DefaultKBLoadGapsKey, Backflow: Backflow{DraftsDir: DefaultBackflowDraftsDir,
			KBMissMinHits: DefaultBackflowKBMissMin, MaxRuns: DefaultBackflowMaxRuns, ServiceUserID: DefaultBackflowServiceUser}}
}

func setAttributionDefaults(v *viper.Viper) {
	d := DefaultAttribution()
	v.SetDefault("attribution.enabled", false)
	v.SetDefault("attribution.timezone", d.Timezone)
	v.SetDefault("attribution.top_n", d.TopN)
	v.SetDefault("attribution.interval", d.Interval)
	v.SetDefault("attribution.kb_load_gaps_key", d.KBLoadGapsKey)
	v.SetDefault("attribution.backflow.enabled", false)
	v.SetDefault("attribution.backflow.drafts_dir", d.Backflow.DraftsDir)
	v.SetDefault("attribution.backflow.kb_miss_min_hits", d.Backflow.KBMissMinHits)
	v.SetDefault("attribution.backflow.max_runs", d.Backflow.MaxRuns)
	v.SetDefault("attribution.backflow.service_user_id", d.Backflow.ServiceUserID)
}

// ErrBadAttribution 表示 attribution 配置非法。
var ErrBadAttribution = errors.New("attribution config invalid")

// Validate 要求时区可加载、TopN / 周期 / 阈值 / 上限 > 0、服务账号非 0、草稿目录非空且为规整路径。
func (a Attribution) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrBadAttribution}, args...)...))
	}
	if _, err := time.LoadLocation(a.Timezone); a.Timezone == "" || err != nil {
		bad("attribution.timezone %q not loadable", a.Timezone)
	}
	if a.TopN <= 0 {
		bad("attribution.top_n must be > 0, got %d", a.TopN)
	}
	if a.Interval <= 0 {
		bad("attribution.interval must be > 0, got %s", a.Interval)
	}
	if len(a.KBLoadGapsKey) > attributionKBLoadGapsKeyMax {
		bad("attribution.kb_load_gaps_key longer than %d", attributionKBLoadGapsKeyMax)
	}
	b := a.Backflow
	if b.DraftsDir == "" || filepath.Clean(b.DraftsDir) != b.DraftsDir {
		bad("attribution.backflow.drafts_dir %q must be a non-empty clean path", b.DraftsDir)
	}
	if b.KBMissMinHits <= 0 {
		bad("attribution.backflow.kb_miss_min_hits must be > 0, got %d", b.KBMissMinHits)
	}
	if b.MaxRuns <= 0 {
		bad("attribution.backflow.max_runs must be > 0, got %d", b.MaxRuns)
	}
	if b.ServiceUserID == 0 {
		bad("attribution.backflow.service_user_id must be > 0")
	}
	return errors.Join(errs...)
}

// Location 返回周界时区（Validate 通过后调用）。
func (a Attribution) Location() (*time.Location, error) {
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: attribution.timezone %q: %w", ErrBadAttribution, a.Timezone, err)
	}
	return loc, nil
}
