package app

import (
	"errors"
	"fmt"

	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1 L5 审批体系装配：待审批详情（GET /approvals/:ticket_id）、驳回原因导出（GET /rejections）、
// notify.send 真实处理器（替换 L4-A 占位）。approval_expiring 扫描在 hitl.go 随 Scanner 装配。

// ErrMissingNotifyTexts 表示 worker / 评测排空构造 outbox 处理器时未注入通知文案（Deps.NotifyTexts）。
var ErrMissingNotifyTexts = errors.New("app: deps.NotifyTexts is required for notify.send (service.LoadNotifyTexts)")

// ApprovalL5 是 L5 新增的服务。Detail 在 HITL.Objects 为空（读不回快照）时为 nil、不挂详情路由。
type ApprovalL5 struct {
	Detail     *service.ApprovalDetailService
	Rejections *service.RejectionExportService
	// ApprovalConfig 是实际生效的 approval 段（零值取 config.DefaultApproval）。
	ApprovalConfig config.Approval
	notifyTexts    *service.NotifyTexts
}

func (a *App) buildApprovalL5(deps Deps) error {
	cfg := deps.Approval
	if cfg == (config.Approval{}) {
		cfg = config.DefaultApproval()
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("app: build approval: %w", err)
	}
	rej, err := service.NewRejectionExportService(repository.RejectionRepo{DB: a.DB}, cfg.RejectionExportMaxSpan)
	if err != nil {
		return fmt.Errorf("app: build approval: %w", err)
	}
	l5 := &ApprovalL5{Rejections: rej, ApprovalConfig: cfg, notifyTexts: deps.NotifyTexts}
	if deps.HITL.Objects != nil {
		raw := repository.RawReader{DB: a.DB, Obj: deps.HITL.Objects, Bucket: deps.HITL.RawBucket}
		if l5.Detail, err = service.NewApprovalDetailService(a.HITL.decisions, raw); err != nil {
			return fmt.Errorf("app: build approval detail: %w", err)
		}
	}
	a.L5 = l5
	return nil
}

// approvalL5Mounts 是 L5 的业务路由（鉴权 / 权限由 routeOps 判定：详情 advice.read，导出 rejection.export）。
func (a *App) approvalL5Mounts() []controller.Mount {
	mounts := []controller.Mount{controller.RejectionRoutes(a.L5.Rejections)}
	if a.L5.Detail != nil {
		mounts = append(mounts, controller.ApprovalDetailRoutes(a.L5.Detail))
	}
	return mounts
}

// notifyHandler 构造 notify.send 处理器：接收人查 user_role / approval_ticket，缺省通道为审计 + 日志（外部通道待对接）。
func (a *App) notifyHandler() (service.OutboxHandler, error) {
	if a.L5 == nil || a.L5.notifyTexts == nil {
		return nil, ErrMissingNotifyTexts
	}
	audit := repository.AuditRepo{DB: a.DB}
	h, err := service.NewNotifyHandler(service.NotifyDeps{Recipients: repository.RecipientRepo{DB: a.DB},
		Channel: service.AuditChannel{Audit: audit, Clock: a.Clock, Logger: a.Logger}, Texts: a.L5.notifyTexts,
		Audit: audit, Clock: a.Clock, Logger: a.Logger})
	if err != nil {
		return nil, fmt.Errorf("app: build notify handler: %w", err)
	}
	return h.Handle, nil
}
