package config

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/viper"
)

// LoaderStrategy 是 configs/loader/{版本}.yaml：一个知识加载策略版本（P2 §6.6）。
// 字段含义与深度校验（排序键、kb_id 格式、上限）在 knowledge.NewStrategy；这里只做结构与必填校验。
type LoaderStrategy struct {
	Version     string                      `mapstructure:"version"`
	Limit       int                         `mapstructure:"limit"`
	Order       []string                    `mapstructure:"order"`
	MustInclude map[string][]LoaderMustRule `mapstructure:"must_include"`
}

// LoaderMustRule 是一条必加规则：kb_id 与 file 二选一；file 可含 {crop}，formulas_only 只取带公式段的条目。
type LoaderMustRule struct {
	KBID         string `mapstructure:"kb_id"`
	File         string `mapstructure:"file"`
	FormulasOnly bool   `mapstructure:"formulas_only"`
}

// LoadLoaderStrategies 读取 dir 下每个版本的 {version}.yaml；任一文件缺失、含未知字段或
// version 字段与文件名不符即报错（调用方据此启动失败）。
func LoadLoaderStrategies(dir string, versions []string) (map[string]*LoaderStrategy, error) {
	if len(versions) == 0 {
		return nil, errors.New("config: loader versions are empty")
	}
	out := make(map[string]*LoaderStrategy, len(versions))
	for _, ver := range versions {
		s, err := loadLoaderStrategy(filepath.Join(dir, ver+".yaml"))
		if err != nil {
			return nil, err
		}
		if s.Version != ver {
			return nil, fmt.Errorf("config: %s: version %q does not match file name", filepath.Join(dir, ver+".yaml"), s.Version)
		}
		out[ver] = s
	}
	return out, nil
}

func loadLoaderStrategy(file string) (*LoaderStrategy, error) {
	v := viper.New()
	v.SetConfigFile(file)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", file, err)
	}
	var s LoaderStrategy
	if err := v.UnmarshalExact(&s); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", file, err)
	}
	if s.Version == "" || s.Limit == 0 || len(s.Order) == 0 {
		return nil, fmt.Errorf("config: %s: version, limit and order are required", file)
	}
	return &s, nil
}
