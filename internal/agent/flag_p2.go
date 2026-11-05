package agent

import (
	"context"
	"errors"
	"fmt"

	"agri-steward/internal/model"
)

// ---- P2 第 3 批：质量优化开关读取接缝（L4 rule_offload.* / L5 enable_loader_v1 / L6 enable_compaction）----

// FarmFlagReader 按农场读开关生效值（30s 进程内缓存）。实现：service.FlagService，app 经 graph.Config.Flags 注入。
// 一个因子一个开关（P2 §7.2）：主链路只经本接口读 P2 开关，不读配置文件或环境变量。
type FarmFlagReader interface {
	Bool(ctx context.Context, key model.FlagKey, scope model.Scope) (bool, error)
	Mode(ctx context.Context, key model.FlagKey, scope model.Scope) (model.FlagMode, error)
}

// errNoFlagReader 表示未装配开关读取（单测 / 旧装配）；调用方按关处理。
var errNoFlagReader = errors.New("agent: flag reader not configured")

// FlagModeOf 读 mode 形态开关；r 为空或读取出错时返回 off（失败关闭）与错误，调用方记日志后按 off 执行。
// r 为空时错误为 errNoFlagReader 的包装，调用方可用 IsNoFlagReader 区分「未装配」与「读取失败」。
func FlagModeOf(ctx context.Context, r FarmFlagReader, key model.FlagKey, scope model.Scope) (model.FlagMode, error) {
	if r == nil {
		return model.FlagModeOff, errNoFlagReader
	}
	m, err := r.Mode(ctx, key, scope)
	if err != nil {
		return model.FlagModeOff, err
	}
	if !m.Valid() {
		return model.FlagModeOff, fmt.Errorf("agent: flag %s mode %q: %w", key, m, model.ErrFlagValue)
	}
	return m, nil
}

// FlagOn 读 bool 形态开关；r 为空或读取出错时返回 false（失败关闭）与错误。
func FlagOn(ctx context.Context, r FarmFlagReader, key model.FlagKey, scope model.Scope) (bool, error) {
	if r == nil {
		return false, errNoFlagReader
	}
	on, err := r.Bool(ctx, key, scope)
	if err != nil {
		return false, err
	}
	return on, nil
}

// IsNoFlagReader 报告错误是否为「未装配开关读取」（此时按关执行属预期，不必告警）。
func IsNoFlagReader(err error) bool { return errors.Is(err, errNoFlagReader) }
