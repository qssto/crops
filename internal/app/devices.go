package app

import (
	"context"
	"fmt"

	"agri-steward/internal/adapter"
	"agri-steward/internal/adapter/devicesim"
	"agri-steward/internal/agent"
	"agri-steward/internal/config"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

// buildDevices 装配设备 Adapter 注册表（P1 §6.3 功能任务 3）：devicesim 开启时把配置的设备类型登记为
// Traced(devicesim) 可开放实现，其余四类真实设备按骨架登记为不可开放。任一登记失败即 Build 失败（启动自检，验收 2）。
// 注册表只决定 to_order 的 dispatch_mode 与给 L4 的 Adapter 查找；本层下发器仍只走 DryRun。
// 生产保护：devicesim 开启时 env=prod（或 env 未给）、base_url 非回环地址都使 Build 失败。
func (a *App) buildDevices(env string, cfg *config.Devices) error {
	reg := adapter.NewRegistry()
	if cfg != nil && cfg.Devicesim.Enabled {
		s := cfg.Devicesim
		if env == "" {
			return fmt.Errorf("app: build devicesim: %w: deps.Env is required when devicesim is enabled",
				config.ErrDevicesimNotAllowed)
		}
		if err := s.CheckDeploy(env); err != nil {
			return fmt.Errorf("app: build devicesim: %w", err)
		}
		client, err := devicesim.New(devicesim.Config{BaseURL: s.BaseURL, Timeout: s.Timeout,
			Caps: adapter.Capabilities{Cancel: s.Capabilities.Cancel, IdemKey: s.Capabilities.IdemKey,
				ExternalQuery: s.Capabilities.ExternalQuery}})
		if err != nil {
			return fmt.Errorf("app: build devicesim: %w", err)
		}
		rec := service.DeviceTraceLog{Log: a.Logger}
		for _, t := range s.DeviceTypes {
			traced, err := adapter.NewTraced(t, client, rec, a.RawStore, a.Clock)
			if err != nil {
				return fmt.Errorf("app: build devicesim %s: %w", t, err)
			}
			if err := reg.Register(t, traced); err != nil {
				return fmt.Errorf("app: register devices: %w", err)
			}
		}
	}
	if err := adapter.RegisterSkeletons(reg); err != nil {
		return fmt.Errorf("app: register devices: %w", err)
	}
	for _, e := range reg.Entries() {
		a.Logger.Info("device adapter registered", "device_type", e.DeviceType, "open", e.Open, "reason", e.Reason,
			"cancel", e.Caps.Cancel, "idem_key", e.Caps.IdemKey, "external_query", e.Caps.ExternalQuery)
	}
	a.Devices = reg
	return nil
}

// deviceDispatchMode 是 agent.DeviceDispatchModeSource 的实现：先按 to_order 规则直读开关（FlagService 不变），
// 开关开但设备类型不在可开放集合时降为 DRY_RUN。开关读失败原样返回（调用方按 DRY_RUN 写定）。
type deviceDispatchMode struct {
	flags agent.DispatchModeSource
	open  func(deviceType string) bool
}

var _ agent.DeviceDispatchModeSource = deviceDispatchMode{}

// DispatchModeFor 实现 agent.DeviceDispatchModeSource。
func (d deviceDispatchMode) DispatchModeFor(ctx context.Context, scope model.Scope,
	deviceType string) (model.DispatchMode, error) {
	mode, err := d.flags.DispatchMode(ctx, scope)
	if err != nil || mode != model.DispatchReal {
		return model.DispatchDryRun, err
	}
	if !d.open(deviceType) {
		return model.DispatchDryRun, nil
	}
	return model.DispatchReal, nil
}

// dispatchModeSource 返回主链路 to_order 用的 dispatch_mode 来源（开关 ∧ 可开放）。
func (a *App) dispatchModeSource() agent.DeviceDispatchModeSource {
	return deviceDispatchMode{flags: a.Flags, open: a.Devices.Openable}
}
