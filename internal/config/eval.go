package config

import (
	"errors"
	"fmt"

	"github.com/spf13/viper"
)

// 评测调度缺省（P2 §6.8 L7-a 技术实现 3、6）：桩模型无限流，QPM 取 5、每 trial 估 3 次 LLM 调用，
// 并发度 floor(5×0.7/3) = 1，与 L7-a 前的顺序执行一致；infra_error 占比超过 5% 判作废。
const (
	DefaultEvalQPM                = 5
	DefaultEvalLLMCallsPerTrial   = 3
	DefaultEvalVoidInfraErrorRate = 0.05
	// MaxEvalVoidInfraErrorRate 是作废阈值上限（路线阈值 5%）：配置只能更严，不能改松。
	MaxEvalVoidInfraErrorRate = 0.05
)

// Eval 是评测调度与作废门禁配置。
type Eval struct {
	// QPM 是模型供应商每分钟请求上限；trial 并发度 = floor(QPM × 0.7 / LLMCallsPerTrial)，至少 1。
	QPM int `mapstructure:"qpm"`
	// LLMCallsPerTrial 是每个 trial 估计的 LLM 调用数。
	LLMCallsPerTrial int `mapstructure:"llm_calls_per_trial"`
	// VoidInfraErrorRate 是 infra_error 占比作废阈值（严格大于即 VOID，写进报告）。
	VoidInfraErrorRate float64 `mapstructure:"void_infra_error_rate"`
}

func setEvalDefaults(v *viper.Viper) {
	v.SetDefault("eval.qpm", DefaultEvalQPM)
	v.SetDefault("eval.llm_calls_per_trial", DefaultEvalLLMCallsPerTrial)
	v.SetDefault("eval.void_infra_error_rate", DefaultEvalVoidInfraErrorRate)
}

// ErrBadEval 表示 eval 配置非法。
var ErrBadEval = errors.New("eval config invalid")

// ValidVoidInfraErrorRate 报告作废阈值是否在 (0, MaxEvalVoidInfraErrorRate] 内（NaN 不在）。
func ValidVoidInfraErrorRate(rate float64) bool {
	return rate > 0 && rate <= MaxEvalVoidInfraErrorRate
}

// Validate 要求 qpm / llm_calls_per_trial 为正、void_infra_error_rate 在 (0, 0.05] 内（超出失败关闭）。
func (e Eval) Validate() error {
	var errs []error
	if e.QPM <= 0 {
		errs = append(errs, fmt.Errorf("%w: eval.qpm must be > 0, got %d", ErrBadEval, e.QPM))
	}
	if e.LLMCallsPerTrial <= 0 {
		errs = append(errs, fmt.Errorf("%w: eval.llm_calls_per_trial must be > 0, got %d", ErrBadEval, e.LLMCallsPerTrial))
	}
	if !ValidVoidInfraErrorRate(e.VoidInfraErrorRate) {
		errs = append(errs, fmt.Errorf("%w: eval.void_infra_error_rate must be in (0, %v], got %v",
			ErrBadEval, MaxEvalVoidInfraErrorRate, e.VoidInfraErrorRate))
	}
	return errors.Join(errs...)
}
