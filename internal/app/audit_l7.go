package app

import (
	"fmt"

	"agri-steward/internal/agent/guardrail/pii"
	"agri-steward/internal/config"
	"agri-steward/internal/controller"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1 L7 审计装配：审计导出（POST/GET /audit/exports + outbox audit.export）、保留期清理（retention）、每日锚点（audit_anchor）。
// 扫描任务只在 cmd/worker 的 Scanner 里运行；导出路由只在 server 挂载。

// AuditL7Deps 是 L7 的可选依赖（零值：三项都不启用，server / worker / 评测照常运行）。
//   - Objects + RawBucket：保留期清理与导出 pii 命中删对象（raw bucket）；缺任一不启用清理与导出。
//   - Anchors：锚点 bucket；为空不启用 audit_anchor。
//   - Alerts：为空取 Deps.Alerts（L7-B 告警出口），仍为空取 service.LogAlertSink。
type AuditL7Deps struct {
	Retention config.Retention
	Audit     config.Audit
	Objects   repository.ObjectRemover
	RawBucket string
	Anchors   service.AnchorStore
	Alerts    model.AlertSink
}

// AuditL7 是 L7 装配结果；未启用的项为 nil。
type AuditL7 struct {
	Export    *service.AuditExportService
	Retention *service.RetentionService
	Anchor    *service.AnchorService
	Alerts    model.AlertSink
}

// buildAuditL7 在 buildHITLServices 之前调用（Scanner 构造时登记 audit_anchor / retention）。
func (a *App) buildAuditL7(deps Deps) error {
	d := deps.L7
	alerts := d.Alerts
	if alerts == nil {
		alerts = deps.Alerts // P1 L7-B 告警出口（buildObserve 已补缺省）
	}
	if alerts == nil {
		alerts = service.LogAlertSink{Logger: a.Logger}
	}
	l7 := &AuditL7{Alerts: alerts}
	chains := repository.AuditChainRepo{DB: a.DB}
	if d.Anchors != nil {
		l7.Anchor = &service.AnchorService{Chains: chains, Store: d.Anchors, Alerts: alerts, Logger: a.Logger}
	}
	if d.Objects != nil && d.RawBucket != "" {
		retention := repository.RetentionRepo{DB: a.DB}
		l7.Retention = &service.RetentionService{Store: retention, Tx: repository.Transactor{DB: a.DB},
			Audit: repository.AuditRepo{DB: a.DB}, Objects: d.Objects, Bucket: d.RawBucket, Logger: a.Logger}
		if deps.Presign != nil {
			maxSpan := d.Audit.ExportMaxSpan
			if maxSpan <= 0 {
				maxSpan = config.DefaultAuditExportMaxSpan
			}
			svc, err := service.NewAuditExportService(service.AuditExportDeps{Store: repository.AuditExportRepo{DB: a.DB},
				Chains: chains, Raw: a.RawStore, RawAccess: repository.RawAccessRepo{DB: a.DB}, Tombstone: retention,
				Objects: d.Objects, RawBucket: d.RawBucket, Presign: deps.Presign, Perms: a.Perms, Alerts: alerts,
				Clock: a.Clock, Logger: a.Logger, MaxSpan: maxSpan, MaskText: pii.Mask, MaskJSON: pii.MaskJSON})
			if err != nil {
				return fmt.Errorf("app: build audit export: %w", err)
			}
			l7.Export = svc
		}
	}
	a.L7 = l7
	return nil
}

// RawKindNames 返回全部 raw_object.kind 的字符串（config.Retention.RequireKinds 校验用）。
func RawKindNames() []string {
	kinds := model.RawKinds()
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// anchorTicker / retentionTicker 返回扫描任务依赖；未启用时返回 nil 接口（避免带类型的 nil）。
func (a *App) anchorTicker() service.AnchorTicker {
	if a.L7 == nil || a.L7.Anchor == nil {
		return nil
	}
	return a.L7.Anchor
}

func (a *App) retentionTicker() service.RetentionTicker {
	if a.L7 == nil || a.L7.Retention == nil {
		return nil
	}
	return a.L7.Retention
}

// auditL7Handlers 是 worker 的 audit.export 处理器（未启用导出时为空表）。
func (a *App) auditL7Handlers() map[string]service.OutboxHandler {
	if a.L7 == nil || a.L7.Export == nil {
		return nil
	}
	return map[string]service.OutboxHandler{repository.TopicAuditExport: a.L7.Export.Handle}
}

// auditL7Mounts 是导出路由（未启用导出时不挂）。
func (a *App) auditL7Mounts() []controller.Mount {
	if a.L7 == nil || a.L7.Export == nil {
		return nil
	}
	return []controller.Mount{controller.AuditExportRoutes(a.L7.Export)}
}
