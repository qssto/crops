package app

import (
	"fmt"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P2 L8 装配：农户采纳 / 差评（feedback，server 挂路由）。归因周报、问题处置与线上回流见 attribution_l8.go。

// L8 是 P2 L8 装配结果。
type L8 struct {
	Feedback *service.FeedbackService
	// Issues 是归因问题处置（平台路由，见 attribution_l8.go）。
	Issues *service.AttributionIssueService
}

// buildL8 装配 L8 的 server 侧服务（反馈只依赖业务库与时钟，总是装配）。
func (a *App) buildL8() error {
	fb, err := service.NewFeedbackService(service.FeedbackDeps{Store: repository.FeedbackRepo{DB: a.DB}, Clock: a.Clock, Mask: pii.Mask})
	if err != nil {
		return fmt.Errorf("app: build feedback: %w", err)
	}
	issues, err := a.buildAttributionIssues()
	if err != nil {
		return err
	}
	a.L8 = &L8{Feedback: fb, Issues: issues}
	return nil
}

// l8Mounts 是 L8 的农场级路由。
func (a *App) l8Mounts() []controller.Mount {
	if a.L8 == nil {
		return nil
	}
	return []controller.Mount{controller.FeedbackRoutes(a.L8.Feedback)}
}
