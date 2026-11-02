package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Devices 是 configs/devices.yaml：设备 Adapter 注册表的装配参数（P1 §6.3）。
// 只有 devicesim（本地模拟平台）一种可开放实现；enabled=false 时四类真实设备全部按骨架登记为不可开放。
type Devices struct {
	Devicesim Devicesim `mapstructure:"devicesim"`
}

// Devicesim 是本地设备模拟平台的接入参数。只用于本地 / 预发演练：DeviceTypes 列出的 work_order.device_type
// 由 devicesim Adapter 承接并进入可开放集合，其余类型仍按骨架登记。
type Devicesim struct {
	Enabled      bool               `mapstructure:"enabled"`
	BaseURL      string             `mapstructure:"base_url"`
	Timeout      time.Duration      `mapstructure:"timeout"`
	DeviceTypes  []string           `mapstructure:"device_types"`
	Capabilities DeviceCapabilities `mapstructure:"capabilities"`
}

// DeviceCapabilities 是平台声明的能力（Registry 按开放规则校验：必须 cancel，idem_key 与 external_query 至少一项）。
type DeviceCapabilities struct {
	Cancel        bool `mapstructure:"cancel"`
	IdemKey       bool `mapstructure:"idem_key"`
	ExternalQuery bool `mapstructure:"external_query"`
}

// LoadDevices 经 Viper 读取设备配置并校验，环境变量（前缀 AS_，如 AS_DEVICESIM_ENABLED=true）覆盖 yaml。
func LoadDevices(file string) (*Devices, error) {
	v := viper.New()
	v.SetConfigFile(file)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, k := range []string{"devicesim.enabled", "devicesim.base_url", "devicesim.timeout"} {
		if err := v.BindEnv(k); err != nil {
			return nil, fmt.Errorf("config: bind %s: %w", k, err)
		}
	}
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", file, err)
	}
	var d Devices
	if err := v.Unmarshal(&d); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", file, err)
	}
	if err := d.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", file, err)
	}
	return &d, nil
}

func (d Devices) validate() error {
	s := d.Devicesim
	if !s.Enabled {
		return nil
	}
	var errs []error
	if s.BaseURL == "" {
		errs = append(errs, errors.New("devicesim.base_url is required when enabled"))
	} else if err := checkLoopback(s.BaseURL); err != nil {
		errs = append(errs, err)
	}
	if s.Timeout <= 0 {
		errs = append(errs, errors.New("devicesim.timeout must be > 0"))
	}
	if len(s.DeviceTypes) == 0 {
		errs = append(errs, errors.New("devicesim.device_types must not be empty when enabled"))
	}
	seen := map[string]bool{}
	for _, t := range s.DeviceTypes {
		if t == "" || seen[t] {
			errs = append(errs, fmt.Errorf("devicesim.device_types: empty or duplicate %q", t))
		}
		seen[t] = true
	}
	return errors.Join(errs...)
}

// EnvProd 是生产环境的 env（configs/app.yaml 的 env 字段）。
const EnvProd = "prod"

// ErrDevicesimNotAllowed 表示 devicesim 的部署保护被触发：生产环境开启，或 base_url 不是本机回环地址。
var ErrDevicesimNotAllowed = errors.New("config: devicesim not allowed")

// CheckDeploy 是 devicesim 的生产保护（app.Build 调用）：未开启直接通过；开启时 env=prod 拒绝，
// 且 base_url 必须指向本机回环地址（localhost / 127.0.0.0/8 / ::1），防止误把模拟平台当真实设备平台接进环境。
func (s Devicesim) CheckDeploy(env string) error {
	if !s.Enabled {
		return nil
	}
	if env == EnvProd {
		return fmt.Errorf("%w: devicesim.enabled=true in env %q (local / staging drills only)", ErrDevicesimNotAllowed, env)
	}
	return checkLoopback(s.BaseURL)
}

// checkLoopback 要求 base_url 是 http(s) 且主机为 localhost 或回环 IP。
func checkLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%w: devicesim.base_url %q must be an http(s) URL", ErrDevicesimNotAllowed, raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%w: devicesim.base_url host %q must be loopback (localhost / 127.0.0.0/8 / ::1)",
		ErrDevicesimNotAllowed, host)
}
