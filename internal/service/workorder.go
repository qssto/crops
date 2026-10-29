// Package service 是业务编排与事务边界。只依赖 context.Context 与 model / repository / agent / adapter，
// 不 import gin、gorm（事务句柄用 repository.DB，即 *gorm.DB 的别名）。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// WorkOrderStore 是 Transition 需要的作业单仓储能力，repository.WorkOrderRepo 实现。
type WorkOrderStore interface {
	GetState(ctx context.Context, tx *repository.DB, ref model.OrderRef) (model.OrderState, error)
	UpdateStatus(ctx context.Context, tx *repository.DB, ref model.OrderRef, from, to model.WorkOrderStatus, rowVer int, now time.Time) error
	InsertTransition(ctx context.Context, tx *repository.DB, t model.OrderTransition) error
}

// EventStore 是同事务追加 run_event 的能力，repository.EventRepo 实现。
type EventStore interface {
	AppendTx(ctx context.Context, tx *repository.DB, e model.RunEvent) (int64, error)
}

// WorkOrderService 持有作业单唯一迁移入口与 order.dispatch 下发器（dispatch 由 SetDispatch 装配）。
type WorkOrderService struct {
	clock    clock.Clock
	orders   WorkOrderStore
	events   EventStore
	dispatch *DispatchDeps
}

// NewWorkOrderService 构造 WorkOrderService。
func NewWorkOrderService(c clock.Clock, orders WorkOrderStore, events EventStore) *WorkOrderService {
	return &WorkOrderService{clock: c, orders: orders, events: events}
}

// transitionPayload 是 ORDER_TRANSITION 事件的 payload（只含控制字段）。
type transitionPayload struct {
	OrderID    uint64                `json:"order_id"`
	From       model.WorkOrderStatus `json:"from"`
	To         model.WorkOrderStatus `json:"to"`
	ReasonCode model.ReasonCode      `json:"reason_code,omitempty"`
}

// Transition 是作业单状态迁移的唯一入口，必须在调用方事务 tx 内执行：
// CanTransition 校验 → UpdateStatus（row_ver 乐观锁）→ InsertTransition → AppendTx(ORDER_TRANSITION)。
// 非法迁移返回 model.ErrIllegalTransition，乐观锁冲突返回 model.ErrConcurrentUpdate。不取 Redis 锁。
func (s *WorkOrderService) Transition(ctx context.Context, tx *repository.DB, ref model.OrderRef,
	to model.WorkOrderStatus, actor model.Actor, reason model.ReasonCode) error {
	return s.transition(ctx, tx, ref, "", to, actor, reason)
}

// TransitionFrom 同 Transition，但要求作业单当前状态为 from（迁移表允许多个来源、业务只接受其一时使用，
// 如复检取消只针对 APPROVED、下发超时只针对 APPROVED）；当前状态不符返回 model.ErrIllegalTransition。
func (s *WorkOrderService) TransitionFrom(ctx context.Context, tx *repository.DB, ref model.OrderRef,
	from, to model.WorkOrderStatus, actor model.Actor, reason model.ReasonCode) error {
	return s.transition(ctx, tx, ref, from, to, actor, reason)
}

// transition 是迁移实现；from 为空表示不限定来源状态。
func (s *WorkOrderService) transition(ctx context.Context, tx *repository.DB, ref model.OrderRef,
	from, to model.WorkOrderStatus, actor model.Actor, reason model.ReasonCode) error {
	cur, err := s.orders.GetState(ctx, tx, ref)
	if err != nil {
		return fmt.Errorf("service: transition order %d: %w", ref.OrderID, err)
	}
	if from != "" && cur.Status != from {
		return fmt.Errorf("service: order %d is %s, want %s -> %s: %w", ref.OrderID, cur.Status, from, to, model.ErrIllegalTransition)
	}
	if !model.CanTransition(cur.Status, to) {
		return fmt.Errorf("service: order %d %s -> %s: %w", ref.OrderID, cur.Status, to, model.ErrIllegalTransition)
	}
	now := s.clock.Now()
	if err := s.orders.UpdateStatus(ctx, tx, ref, cur.Status, to, cur.RowVer, now); err != nil {
		return fmt.Errorf("service: transition order %d: %w", ref.OrderID, err)
	}
	if err := s.orders.InsertTransition(ctx, tx, model.OrderTransition{
		Ref: ref, RunID: cur.RunID, From: cur.Status, To: to, Actor: actor, ReasonCode: reason, CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("service: transition order %d: %w", ref.OrderID, err)
	}
	payload, err := json.Marshal(transitionPayload{OrderID: ref.OrderID, From: cur.Status, To: to, ReasonCode: reason})
	if err != nil {
		return fmt.Errorf("service: encode transition payload: %w", err)
	}
	if _, err := s.events.AppendTx(ctx, tx, model.RunEvent{
		TenantID: ref.TenantID, FarmID: ref.FarmID, RunID: cur.RunID, Type: model.EventOrderTransition,
		Actor: actor.Type, ActorID: actor.ID, Payload: payload, CreatedAt: now,
	}); err != nil {
		return fmt.Errorf("service: transition order %d: %w", ref.OrderID, err)
	}
	return nil
}
