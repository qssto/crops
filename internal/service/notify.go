package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"agri-steward/internal/clock"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L5 notify.send 处理器（替换 L4-A 的 NotifyPlaceholder）：按 kind 解析接收人 → 取文案 → 经 NotifyChannel 投递。
//   - PENDING_APPROVAL / EXPIRING_SOON：本农场 required_role 内用户，排除发起人。
//   - RECHECK_RETURNED：发起人 + required_role 用户（新票；payload 无 ticket_id 时取 new_ticket_id）。
//   - DEVICE_OFFLINE：本农场 farm_manager + 发起人（ticket_id 可缺省，取该单最新 ORDER 票）。
// 无接收人 → 审计 NOTIFY_SEND result=FAILED + Warn 后返回 nil（置 DONE，不空转）；通道失败返回错误走 outbox 退避；
// 未知 kind / payload 不可解析 → ErrBadOutboxPayload。

// Notification 是投递给通道的一条通知（已解析接收人与文案）。
type Notification struct {
	Scope    model.Scope
	Kind     model.NotifyKind
	OrderID  uint64
	TicketID uint64
	RunID    uint64
	UserIDs  []uint64
	TextKey  string
	Text     string
	DedupKey string
}

// NotifyChannel 是通知投递通道。缺省实现 AuditChannel（审计 + 日志）；短信 / 企业微信 / App 推送等外部通道
// 实现本接口即可接入（待对接）。与 agent.UserChannel 的区别：UserChannel 是主链路对提问人的同步回话，
// NotifyChannel 是 worker 经 outbox 异步发给农场内其他角色的通知，二者不共用实现。
// Send 返回错误即本次投递失败，outbox 按退避重试（实现须按 DedupKey 幂等）。
type NotifyChannel interface {
	Send(ctx context.Context, n Notification) error
}

// NotifyRecipients 是接收人查询，repository.RecipientRepo 实现。
type NotifyRecipients interface {
	FarmUsersByRoles(ctx context.Context, scope model.Scope, roles []model.Role) ([]uint64, error)
	TicketParties(ctx context.Context, scope model.Scope, orderID, ticketID uint64) (repository.TicketParties, error)
}

// NotifyDeps 是 NotifyHandler 的依赖（全部必填）。
type NotifyDeps struct {
	Recipients NotifyRecipients
	Channel    NotifyChannel
	Texts      *NotifyTexts
	Audit      AuditAppender
	Clock      clock.Clock
	Logger     *slog.Logger
}

// NotifyHandler 是 outbox notify.send 的处理器。
type NotifyHandler struct{ d NotifyDeps }

// NewNotifyHandler 校验依赖。
func NewNotifyHandler(d NotifyDeps) (*NotifyHandler, error) {
	if d.Recipients == nil || d.Channel == nil || d.Texts == nil || d.Audit == nil || d.Clock == nil || d.Logger == nil {
		return nil, fmt.Errorf("service: notify handler: %w", ErrDispatchNotConfigured)
	}
	return &NotifyHandler{d: d}, nil
}

// notifyPayload 是 envelope + 复检退回的 new_ticket_id（L4-A 的 payload 只带 old / new_ticket_id）。
type notifyPayload struct {
	model.NotifyEnvelope
	NewTicketID uint64 `json:"new_ticket_id"`
}

// ticketless 是 ticket_id 可缺省的通知类型。
func ticketless(k model.NotifyKind) bool { return k == model.NotifyDeviceOffline }

// Handle 实现 OutboxHandler。
func (h *NotifyHandler) Handle(ctx context.Context, msg repository.OutboxMessage) error {
	var p notifyPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return fmt.Errorf("%w: notify.send outbox %d: %w", ErrBadOutboxPayload, msg.ID, err)
	}
	if p.TicketID == 0 {
		p.TicketID = p.NewTicketID
	}
	text, known := h.d.Texts.Render(p.Kind, p.OrderID, p.TicketID)
	switch {
	case !known:
		return fmt.Errorf("%w: notify.send outbox %d: unknown kind %q", ErrBadOutboxPayload, msg.ID, p.Kind)
	case p.OrderID == 0 || (p.TicketID == 0 && !ticketless(p.Kind)):
		return fmt.Errorf("%w: notify.send outbox %d: %s needs order_id and ticket_id", ErrBadOutboxPayload, msg.ID, p.Kind)
	}
	n := Notification{Scope: model.Scope{TenantID: msg.TenantID, FarmID: msg.FarmID}, Kind: p.Kind, OrderID: p.OrderID,
		TicketID: p.TicketID, RunID: p.RunID, TextKey: NotifyTextKey(p.Kind), Text: text, DedupKey: msg.DedupKey}
	users, err := h.recipients(ctx, n)
	if err != nil {
		return fmt.Errorf("service: notify.send outbox %d recipients: %w", msg.ID, err)
	}
	if len(users) == 0 {
		return h.noRecipient(ctx, msg, n)
	}
	n.UserIDs = users
	if err := h.d.Channel.Send(ctx, n); err != nil {
		return fmt.Errorf("service: notify.send outbox %d channel: %w", msg.ID, err)
	}
	return nil
}

// recipients 按 kind 解析接收人（去重、升序）。票据查无（ErrNotFound）视为无当事人，不报错。
func (h *NotifyHandler) recipients(ctx context.Context, n Notification) ([]uint64, error) {
	parties, err := h.d.Recipients.TicketParties(ctx, n.Scope, n.OrderID, n.TicketID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	roles := parties.RequiredRole
	if n.Kind == model.NotifyDeviceOffline {
		roles = []model.Role{model.RoleFarmManager}
	}
	// 待审批 / 即将过期只发给审批人（发起人不能审自己的单）；复检退回 / 设备离线额外通知发起人。
	approversOnly := n.Kind == model.NotifyPendingApproval || n.Kind == model.NotifyExpiringSoon
	users, err := h.d.Recipients.FarmUsersByRoles(ctx, n.Scope, roles)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, 0, len(users)+1)
	for _, u := range users {
		if approversOnly && u == parties.InitiatorID {
			continue
		}
		out = append(out, u)
	}
	if !approversOnly && parties.InitiatorID != 0 {
		out = append(out, parties.InitiatorID)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// noRecipient 写 FAILED 审计 + Warn 后返回 nil（消息置 DONE）；审计写失败返回错误（退避，不吞）。
func (h *NotifyHandler) noRecipient(ctx context.Context, msg repository.OutboxMessage, n Notification) error {
	payload, err := json.Marshal(map[string]any{"kind": n.Kind, "order_id": n.OrderID, "ticket_id": n.TicketID,
		"text_key": n.TextKey, "reason": "NO_RECIPIENT"})
	if err != nil {
		return fmt.Errorf("service: notify.send outbox %d: %w", msg.ID, err)
	}
	if err := h.d.Audit.Append(ctx, notifyAudit(n, payload, model.AuditResultFailed, h.d.Clock)); err != nil {
		return fmt.Errorf("service: notify.send outbox %d audit: %w", msg.ID, err)
	}
	h.d.Logger.WarnContext(ctx, "notify.send: no recipient", "outbox_id", msg.ID, "kind", n.Kind,
		"order_id", n.OrderID, "ticket_id", n.TicketID, "farm_id", n.Scope.FarmID)
	return nil
}

func notifyAudit(n Notification, payload json.RawMessage, result string, clk clock.Clock) model.AuditRecord {
	var runID *uint64
	if n.RunID != 0 {
		runID = &n.RunID
	}
	return repository.NewAuditRecord(n.Scope, runID, model.Actor{Type: model.ActorSystem}, model.AuditNotifySend,
		"order:"+strconv.FormatUint(n.OrderID, 10), payload, result, clk.Now())
}

// AuditChannel 是缺省通道：写审计 NOTIFY_SEND（user_ids / kind / text_key，不含正文与个人信息）+ Info 日志。
// 审计写失败返回错误（outbox 退避）。
type AuditChannel struct {
	Audit  AuditAppender
	Clock  clock.Clock
	Logger *slog.Logger
}

// Send 实现 NotifyChannel。
func (c AuditChannel) Send(ctx context.Context, n Notification) error {
	if c.Audit == nil || c.Clock == nil || c.Logger == nil {
		return fmt.Errorf("service: audit channel: %w", ErrDispatchNotConfigured)
	}
	payload, err := json.Marshal(map[string]any{"kind": n.Kind, "order_id": n.OrderID, "ticket_id": n.TicketID,
		"user_ids": n.UserIDs, "text_key": n.TextKey, "dedup_key": n.DedupKey})
	if err != nil {
		return fmt.Errorf("service: audit channel: %w", err)
	}
	if err := c.Audit.Append(ctx, notifyAudit(n, payload, model.AuditResultOK, c.Clock)); err != nil {
		return fmt.Errorf("service: audit channel: %w", err)
	}
	c.Logger.InfoContext(ctx, "notify.send delivered", "channel", "audit", "kind", n.Kind, "order_id", n.OrderID,
		"ticket_id", n.TicketID, "farm_id", n.Scope.FarmID, "recipients", len(n.UserIDs), "text_key", n.TextKey)
	return nil
}
