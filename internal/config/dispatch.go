package config

import (
	"errors"
	"time"

	"github.com/spf13/viper"
)

// Dispatch 是下发与重试配置（configs/app.yaml 的 dispatch 段，P1 §6.5）：
// 下发器（L4-A）按 MaxAttempts 校验 order.dispatch 的 attempt，回执侧（L4-B）按同一个值判断 NOT_EXECUTED 是否再下发一次；
// ReceiptTimeout 是回执时限（dispatch_attempt.receipt_due_at = 下发 / 受理时刻 + ReceiptTimeout）；
// DeviceWaitMax 是选不到设备时的最长等待：作业单无 dispatch_deadline_at 时，自 order.dispatch 消息入队（outbox.created_at）起
// 超过该时长仍无可用设备即放弃下发（首次 APPROVED → EXPIRED、重试 DISPATCHED → FAILED，reason DEVICE_UNAVAILABLE）。
// 环境变量覆盖：AS_DISPATCH_MAX_ATTEMPTS、AS_DISPATCH_RECEIPT_TIMEOUT、AS_DISPATCH_DEVICE_WAIT_MAX。
type Dispatch struct {
	MaxAttempts    int           `mapstructure:"max_attempts"`
	ReceiptTimeout time.Duration `mapstructure:"receipt_timeout"`
	DeviceWaitMax  time.Duration `mapstructure:"device_wait_max"`
}

// 下发配置缺省值（yaml 未写时生效）。
const (
	DefaultDispatchMaxAttempts    = 2
	DefaultDispatchReceiptTimeout = 30 * time.Minute
	// DefaultDispatchDeviceWaitMax 与审批生效到 dispatch_deadline_at 的默认时长（service.DefaultDispatchWindow）一致。
	DefaultDispatchDeviceWaitMax = 2 * time.Hour
)

// DefaultDispatch 是缺省下发配置（评测与测试装配用）。
func DefaultDispatch() Dispatch {
	return Dispatch{MaxAttempts: DefaultDispatchMaxAttempts, ReceiptTimeout: DefaultDispatchReceiptTimeout,
		DeviceWaitMax: DefaultDispatchDeviceWaitMax}
}

// setDispatchDefaults 注册 dispatch 段缺省值（同时让 AutomaticEnv 能覆盖未写进 yaml 的键）。
func setDispatchDefaults(v *viper.Viper) {
	v.SetDefault("dispatch.max_attempts", DefaultDispatchMaxAttempts)
	v.SetDefault("dispatch.receipt_timeout", DefaultDispatchReceiptTimeout)
	v.SetDefault("dispatch.device_wait_max", DefaultDispatchDeviceWaitMax)
}

// Validate 校验 max_attempts ≥ 1、receipt_timeout > 0、device_wait_max > 0。
func (d Dispatch) Validate() error {
	var errs []error
	if d.MaxAttempts < 1 {
		errs = append(errs, errors.New("config: dispatch.max_attempts must be >= 1"))
	}
	if d.ReceiptTimeout <= 0 {
		errs = append(errs, errors.New("config: dispatch.receipt_timeout must be > 0"))
	}
	if d.DeviceWaitMax <= 0 {
		errs = append(errs, errors.New("config: dispatch.device_wait_max must be > 0"))
	}
	return errors.Join(errs...)
}
