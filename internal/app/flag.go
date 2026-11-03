package app

import (
	"fmt"

	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// buildFlags 装配 P1-L0 开关服务（feature_flag 三层合并 + 30s 缓存 + flags:changed 失效）。
// enable_robot_dispatch 置开的 agronomist 前置检查直接查 user_role。
// 须在 NewAgent 之前：主链路 to_order 经 Flags.DispatchMode 取下发模式。cmd/server 启动 Flags.Run。
func (a *App) buildFlags() error {
	svc, err := service.NewFlagService(service.FlagDeps{Store: repository.FlagRepo{DB: a.DB},
		Tx: repository.Transactor{DB: a.DB}, Audit: repository.AuditRepo{DB: a.DB},
		Pub: repository.RedisPublisher{Client: a.Redis}, Sub: repository.RedisSubscriber{Client: a.Redis},
		FarmRoles: repository.UserRoleRepo{DB: a.DB}, Keys: a.Keys, Clock: a.Clock, Logger: a.Logger,
		Observer: a.Observer}) // P1 L7-B：as_flag_change_total
	if err != nil {
		return fmt.Errorf("app: build flags: %w", err)
	}
	a.Flags = svc
	return nil
}
