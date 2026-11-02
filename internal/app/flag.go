package app

import (
	"fmt"

	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// buildFlags 装配 P1-L0 开关服务（feature_flag 三层合并 + 30s 缓存 + flags:changed 失效）。
// 须在 NewAgent 之前：主链路 to_order 经 Flags.DispatchMode 取下发模式。cmd/server 启动 Flags.Run。
func (a *App) buildFlags(deps Deps) error {
	svc, err := service.NewFlagService(service.FlagDeps{Store: repository.FlagRepo{DB: a.DB},
		Tx: repository.Transactor{DB: a.DB}, Audit: repository.AuditRepo{DB: a.DB},
		Pub: repository.RedisPublisher{Client: a.Redis}, Sub: repository.RedisSubscriber{Client: a.Redis},
		FarmRoles: deps.FarmRoles, Keys: a.Keys, Clock: a.Clock, Logger: a.Logger})
	if err != nil {
		return fmt.Errorf("app: build flags: %w", err)
	}
	a.Flags = svc
	return nil
}
