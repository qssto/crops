package app

import (
	"context"
	"fmt"

	"agri-steward/internal/adapter"
	"agri-steward/internal/controller"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

// P1-L4 接缝：L4-B（回执 / 中止 / 对账 / 回写）在本文件装配，L4-A（cmd/worker 与下发器）只调用这四个方法。
// a.OrderOps 由 buildOrderOps 装配；回执 HMAC 密钥（Deps.ReceiptSecret）为空时不挂回执 webhook。

// orderOpsMounts 是 L4-B 的用户路由（cancel、reconcile、待对账列表），由 Handler 挂在农场分组上。
func (a *App) orderOpsMounts() []controller.Mount {
	if a.OrderOps == nil {
		return nil
	}
	return []controller.Mount{controller.OrderOpsRoutes(a.OrderOps)}
}

// orderOpsEngineMounts 是 L4-B 的设备侧路由（回执 webhook，HMAC 签名校验，不经用户鉴权），由 Handler 挂在根路由上。
// 密钥未配置时返回 nil 并告警（buildOrderOps 已记录一次）。
func (a *App) orderOpsEngineMounts() []controller.EngineMount {
	if a.OrderOps == nil || !a.receiptWebhook {
		return nil
	}
	return []controller.EngineMount{controller.ReceiptRoutes(a.OrderOps)}
}

// OrderOpsHandlers 是 L4-B 的 outbox topic 处理器（order.cancel / field.history.append），
// 由 cmd/worker 与 order.dispatch 处理器合并成同一张处理器表。
func (a *App) OrderOpsHandlers() map[string]service.OutboxHandler {
	if a.OrderOps == nil {
		return nil
	}
	return map[string]service.OutboxHandler{
		repository.TopicOrderCancel:        a.OrderOps.HandleCancel,
		repository.TopicFieldHistoryAppend: a.OrderOps.HandleFieldHistory,
	}
}

// OrderOpsLoops 是 L4-B 的 worker 后台循环（回执超时扫描 lock:scan:receipt_due），
// cmd/worker 为每个循环起一个 goroutine，ctx 取消后等待全部返回。
func (a *App) OrderOpsLoops() []func(context.Context) {
	if a.OrderOps == nil {
		return nil
	}
	return []func(context.Context){a.OrderOps.RunReceiptDue}
}

// receiptStore 组合回执侧仓储：DeviceReceiptRepo（回执 / 超时扫描 / outbox）、DispatchAttemptRepo（回执 UPDATE / 待对账 /
// 任务号反查）与 DispatchRepo（作业单读取），实现 service.ReceiptStore 与 service.ReceiptLookup。
type receiptStore struct {
	repository.DeviceReceiptRepo
	repository.DispatchAttemptRepo
	orders repository.DispatchRepo
}

// OrderForDispatch 读作业单（DispatchRepo）。
func (s receiptStore) OrderForDispatch(ctx context.Context, db *repository.DB, ref model.OrderRef) (repository.DispatchOrder, error) {
	return s.orders.OrderForDispatch(ctx, db, ref)
}

// registryDevices 把 adapter.Registry 桥接为 service.ReceiptDevices：按 device_type 取 Adapter，
// 作业单归属经 WithTraceOrder 进 device trace。fixed 非空时（评测：Deps.Adapter 是 env.RecordingAdapter，
// 实现完整 DeviceAdapter）所有设备类型都用它；生产 Deps.Adapter 为 MockAdapter（只有 DryRun），走注册表。
type registryDevices struct {
	reg   *adapter.Registry
	fixed adapter.DeviceAdapter
}

func (d registryDevices) get(ctx context.Context, t service.DeviceTarget) (context.Context, adapter.DeviceAdapter, error) {
	ad := d.fixed
	if ad == nil {
		var err error
		if ad, err = d.reg.Get(t.DeviceType); err != nil {
			return ctx, nil, fmt.Errorf("app: receipt device %s: %w", t.DeviceType, err)
		}
	}
	return adapter.WithTraceOrder(ctx, adapter.TraceOrder{TenantID: t.Ref.TenantID, FarmID: t.Ref.FarmID,
		OrderID: t.Ref.OrderID, RunID: t.RunID}), ad, nil
}

// QueryStatus 实现 service.ReceiptDevices。
func (d registryDevices) QueryStatus(ctx context.Context, t service.DeviceTarget, idemKey, jobID string) (model.DeviceReceipt, error) {
	ctx, ad, err := d.get(ctx, t)
	if err != nil {
		return model.DeviceReceipt{}, err
	}
	return ad.QueryStatus(ctx, adapter.StatusRef{IdemKey: idemKey, DeviceJobID: jobID})
}

// Cancel 实现 service.ReceiptDevices。
func (d registryDevices) Cancel(ctx context.Context, t service.DeviceTarget, jobID string) (model.DeviceReceipt, error) {
	ctx, ad, err := d.get(ctx, t)
	if err != nil {
		return model.DeviceReceipt{}, err
	}
	return ad.Cancel(ctx, jobID)
}

// buildOrderOps 装配回执 / 中止 / 对账 / 回写服务（须在 buildAccess、buildDevices 之后）。
// field.history 写端缺省为 mock（内存幂等）；回执密钥为空时 webhook 不挂载并告警。
func (a *App) buildOrderOps(deps Deps) error {
	history := deps.FieldHistory
	if history == nil {
		history = service.NewMemFieldHistory()
	}
	store := receiptStore{DeviceReceiptRepo: repository.DeviceReceiptRepo{DB: a.DB},
		DispatchAttemptRepo: repository.DispatchAttemptRepo{DB: a.DB}, orders: repository.NewDispatchRepo(a.DB)}
	audit := repository.AuditRepo{DB: a.DB}
	devices := registryDevices{reg: a.Devices}
	if full, ok := a.Adapter.(adapter.DeviceAdapter); ok {
		devices.fixed = full
	}
	svc, err := service.NewOrderOpsService(service.OrderOpsDeps{Store: store, Lookup: store,
		Tickets: repository.NewDecisionRepo(a.DB, a.Clock), Tx: repository.Transactor{DB: a.DB},
		Orders: a.WorkOrders, Devices: devices,
		Idem: repository.NewRedisIdem(a.Redis), Locks: a.Locker, Keys: a.Keys, Raw: a.RawStore,
		Audit: audit, AuditTx: audit, Denials: a.Denials, Tenants: repository.UserRoleRepo{DB: a.DB},
		History: history, Secret: deps.ReceiptSecret, MaxAttempts: a.DispatchConfig.MaxAttempts,
		Clock: a.Clock, Logger: a.Logger})
	if err != nil {
		return fmt.Errorf("app: build order ops: %w", err)
	}
	a.OrderOps = svc
	a.FieldHistory = history
	a.receiptWebhook = len(deps.ReceiptSecret) > 0
	if !a.receiptWebhook {
		a.Logger.Warn("device receipt webhook not mounted: AS_DEVICE_RECEIPT_SECRET is not configured")
	}
	return nil
}
