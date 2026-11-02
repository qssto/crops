package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// field.history 回写（§6.5 第 9 步处理器）：outbox(field.history.append) → FieldHistoryWriter（mcp-field 写端）。
// 回写只经 outbox 处理器调用，不注册为 MCP 工具、不进工具策略，模型不可见（no_model_actuate）。

// ErrFieldHistoryUnwired 表示 real 模式的 mcp-field 尚无写接口（待对接）；处理器失败关闭为告警，outbox 退避重试。
var ErrFieldHistoryUnwired = errors.New("FIELD_HISTORY_UNWIRED")

// alertFieldHistory 是 field.history 回写失败告警。
const alertFieldHistory = "FIELD_HISTORY_WRITE_FAILED"

// FieldHistoryRecord 是一条作业完成记录（order_id 作外部键，同一 (tenant, farm, order) 重复写入幂等）。
type FieldHistoryRecord struct {
	TenantID   uint64
	FarmID     uint64
	FieldID    uint64
	OrderID    uint64
	TicketID   uint64
	WorkType   string
	Status     model.WorkOrderStatus
	DoneAreaMu *float64
	DoneAt     time.Time
}

// FieldHistoryWriter 是 mcp-field 的作业记录写端（不对模型可见）。
type FieldHistoryWriter interface {
	AppendWorkDone(ctx context.Context, r FieldHistoryRecord) error
}

// MemFieldHistory 是 mock 模式的写端：内存保存，按 (tenant, farm, order) 幂等（重复投递不产生重复记录）。
type MemFieldHistory struct {
	mu   sync.Mutex
	recs map[model.OrderRef]FieldHistoryRecord
}

// NewMemFieldHistory 构造空的 mock 写端。
func NewMemFieldHistory() *MemFieldHistory {
	return &MemFieldHistory{recs: map[model.OrderRef]FieldHistoryRecord{}}
}

// AppendWorkDone 写入一条记录；同一作业单已存在则忽略。
func (m *MemFieldHistory) AppendWorkDone(_ context.Context, r FieldHistoryRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := model.OrderRef{TenantID: r.TenantID, FarmID: r.FarmID, OrderID: r.OrderID}
	if _, ok := m.recs[k]; !ok {
		m.recs[k] = r
	}
	return nil
}

// Records 返回某农场某地块的记录（按 order_id 升序；fieldID 为 0 取全部地块）。
func (m *MemFieldHistory) Records(tenantID, farmID, fieldID uint64) []FieldHistoryRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []FieldHistoryRecord
	for k, r := range m.recs {
		if k.TenantID == tenantID && k.FarmID == farmID && (fieldID == 0 || r.FieldID == fieldID) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

// UnwiredFieldHistory 是 real 模式占位：mcp-field 写接口待对接，一律 ErrFieldHistoryUnwired（失败关闭）。
type UnwiredFieldHistory struct{}

// AppendWorkDone 总是失败。
func (UnwiredFieldHistory) AppendWorkDone(context.Context, FieldHistoryRecord) error {
	return fmt.Errorf("%w: mcp-field real write endpoint 待对接", ErrFieldHistoryUnwired)
}

// HandleFieldHistory 是 outbox(field.history.append) 处理器：解析消息 → FieldHistoryWriter.AppendWorkDone。
// 写失败返回错误（outbox 退避重试）并告警；消息本身非法时也返回错误（转人工，不静默丢弃）。
func (s *OrderOpsService) HandleFieldHistory(ctx context.Context, msg repository.OutboxMessage) error {
	var p fieldHistoryMessage
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return fmt.Errorf("service: field.history outbox %d payload: %w", msg.ID, err)
	}
	if p.OrderID == 0 || msg.TenantID == 0 || msg.FarmID == 0 {
		return fmt.Errorf("%w: field.history outbox %d missing tenant / farm / order", ErrBadInput, msg.ID)
	}
	err := s.d.History.AppendWorkDone(ctx, FieldHistoryRecord{TenantID: msg.TenantID, FarmID: msg.FarmID, FieldID: p.FieldID,
		OrderID: p.OrderID, TicketID: p.TicketID, WorkType: p.WorkType, Status: p.Status, DoneAreaMu: p.DoneAreaMu, DoneAt: p.DoneAt})
	if err != nil {
		s.d.Logger.ErrorContext(ctx, "field.history write failed", "alert", alertFieldHistory, "order_id", p.OrderID,
			"outbox_id", msg.ID, "attempts", msg.Attempts, slog.Any("err", err))
		return fmt.Errorf("service: field.history order %d: %w", p.OrderID, err)
	}
	return nil
}
