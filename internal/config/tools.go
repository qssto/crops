package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// 工具 Server 运行模式（configs/tools.yaml 的 tools.{server}.mode）。
const (
	ToolModeMock = "mock"
	ToolModeReal = "real"
)

// Tools 是 configs/tools.yaml：四个 MCP Server 各自的 mock / real 开关、超时与策略等级。
// mock → real 切换只改 mode，不改业务代码（P0 §6.3 验收 4）。
type Tools struct {
	MockDir string     `mapstructure:"mock_dir"` // mock 数据目录，内含 {server}.yaml
	Weather ToolServer `mapstructure:"weather"`
	Crop    ToolServer `mapstructure:"crop"`
	Field   ToolServer `mapstructure:"field"`
	Device  ToolServer `mapstructure:"device"`
}

// ToolServer 是单个 Server 的配置。
type ToolServer struct {
	Mode    string        `mapstructure:"mode"`    // mock / real
	Timeout time.Duration `mapstructure:"timeout"` // 单次调用超时（gateway 与 real HTTP 客户端共用）
	Level   string        `mapstructure:"level"`   // 策略等级 read / write / actuate；P0 四个 Server 均为 read
	Real    RealUpstream  `mapstructure:"real"`
}

// RealUpstream 是 real 实现的上游参数。APIKey 是密钥，只从环境变量 AS_TOOLS_{SERVER}_REAL_API_KEY 读取。
type RealUpstream struct {
	BaseURL string `mapstructure:"base_url"`
	APIKey  string `mapstructure:"api_key"` // secret
}

var (
	toolServerNames = []string{"weather", "crop", "field", "device"}
	toolModes       = map[string]bool{ToolModeMock: true, ToolModeReal: true}
	toolLevels      = map[string]bool{"read": true, "write": true, "actuate": true}
)

// LoadTools 经 Viper 读取工具配置并校验，环境变量（前缀 AS_，如 AS_TOOLS_WEATHER_MODE=real）覆盖 yaml。
func LoadTools(file string) (*Tools, error) {
	v := viper.New()
	v.SetConfigFile(file)
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", file, err)
	}
	for _, name := range toolServerNames {
		key := "tools." + name + ".real.api_key"
		if v.InConfig(key) {
			return nil, fmt.Errorf("%w: %s in %s", ErrSecretInFile, key, file)
		}
		if err := v.BindEnv(key); err != nil {
			return nil, fmt.Errorf("config: bind %s: %w", key, err)
		}
	}
	var wrapper struct {
		Tools Tools `mapstructure:"tools"`
	}
	if err := v.Unmarshal(&wrapper); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", file, err)
	}
	if err := wrapper.Tools.validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", file, err)
	}
	return &wrapper.Tools, nil
}

// Servers 按固定顺序返回 server 名与配置。
func (t Tools) Servers() []NamedToolServer {
	return []NamedToolServer{
		{Name: "weather", ToolServer: t.Weather},
		{Name: "crop", ToolServer: t.Crop},
		{Name: "field", ToolServer: t.Field},
		{Name: "device", ToolServer: t.Device},
	}
}

// NamedToolServer 是带名字的 Server 配置。
type NamedToolServer struct {
	Name string
	ToolServer
}

func (t Tools) validate() error {
	var errs []error
	needMockDir := false
	for _, s := range t.Servers() {
		if !toolModes[s.Mode] {
			errs = append(errs, fmt.Errorf("tools.%s.mode %q not in [mock real]", s.Name, s.Mode))
		}
		if s.Timeout <= 0 {
			errs = append(errs, fmt.Errorf("tools.%s.timeout must be > 0", s.Name))
		}
		if !toolLevels[s.Level] {
			errs = append(errs, fmt.Errorf("tools.%s.level %q not in [read write actuate]", s.Name, s.Level))
		}
		needMockDir = needMockDir || s.Mode == ToolModeMock
	}
	if needMockDir && t.MockDir == "" {
		errs = append(errs, errors.New("tools.mock_dir is required when any server runs in mock mode"))
	}
	return errors.Join(errs...)
}
