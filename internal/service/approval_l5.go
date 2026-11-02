package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/agent/harness/policy"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// P1 L5 审批体系在 ApprovalService 上的追加：规则定级输入、快照高亮与差异、Activate 同事务待审批通知。

// diffLookback 是找「上一张同类完成单」的回看窗口（LocalOrderSource 的 since = now − diffLookback）。
const diffLookback = 365 * 24 * time.Hour

// DiffNoPrevious 是 OrderDiff.Note 的取值：同地块没有上一张同类 REAL 完成单，差异为空。
const DiffNoPrevious = "NO_PREVIOUS_ORDER"

// OrderHighlights 是审批页高亮字段（§6.6 功能任务 4）：药剂、用量（payload 原样）与时间窗。
type OrderHighlights struct {
	Product    json.RawMessage  `json:"product,omitempty"`
	DosePerMu  json.RawMessage  `json:"dose_per_mu,omitempty"`
	AreaMu     *float64         `json:"area_mu,omitempty"`
	TimeWindow model.TimeWindow `json:"time_window"`
}

// FieldDiff 是一个 payload 顶层字段与上一张同类单的差异；一侧缺失为 null。
type FieldDiff struct {
	Field string          `json:"field"`
	Prev  json.RawMessage `json:"prev"`
	Cur   json.RawMessage `json:"cur"`
}

// OrderDiff 是本单与同地块上一张同类 REAL 完成单（DONE / PARTIALLY_DONE）的 payload 差异；没有上一张时
// PrevOrderID = 0、Note = DiffNoPrevious。
type OrderDiff struct {
	PrevOrderID uint64      `json:"prev_order_id,omitempty"`
	PrevStatus  string      `json:"prev_status,omitempty"`
	Note        string      `json:"note,omitempty"`
	Fields      []FieldDiff `json:"fields"`
}

// orderPayload 是定级与高亮读取的 payload 字段（其余字段忽略）。
type orderPayload struct {
	Product   json.RawMessage `json:"product"`
	DosePerMu json.RawMessage `json:"dose_per_mu"`
	AreaMu    *float64        `json:"area_mu"`
}

// parsePayload 解析作业单 payload；空 payload 视为无字段，非对象 / 字段类型不对返回 ErrBadApproval。
func parsePayload(raw json.RawMessage) (orderPayload, error) {
	var p orderPayload
	if len(bytes.TrimSpace(raw)) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("%w: payload: %w", ErrBadApproval, err)
	}
	return p, nil
}

// assessRisk 按 risk_matrix v2 定级：作业类型、payload.area_mu（缺失时矩阵记 LARGE_AREA:unknown → HIGH）与复核后的校验行。
func (s *ApprovalService) assessRisk(wt model.WorkType, payload json.RawMessage, checks []model.ConstraintCheck) (policy.Risk, error) {
	p, err := parsePayload(payload)
	if err != nil {
		return policy.Risk{}, err
	}
	return s.d.Risk.Assess(policy.RiskInput{WorkType: wt, AreaMu: p.AreaMu, Checks: checks})
}

// highlights 取审批页高亮字段（payload 不可解析返回 ErrBadApproval）。
func highlights(o model.WorkOrder) (OrderHighlights, error) {
	p, err := parsePayload(o.Payload)
	if err != nil {
		return OrderHighlights{}, err
	}
	return OrderHighlights{Product: p.Product, DosePerMu: p.DosePerMu, AreaMu: p.AreaMu, TimeWindow: o.TimeWindow}, nil
}

// orderDiff 找同地块上一张同类 REAL 完成单（updated_at 最新，同时刻取 id 大者）并逐个 payload 顶层字段比较。
func (s *ApprovalService) orderDiff(ctx context.Context, o model.WorkOrder, now time.Time) (OrderDiff, error) {
	none := OrderDiff{Note: DiffNoPrevious, Fields: []FieldDiff{}}
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	recent, err := s.d.History.RecentOrders(ctx, scope, o.FieldID, o.OrderID, now.Add(-diffLookback))
	if err != nil {
		return OrderDiff{}, err
	}
	var prev *int
	for i, r := range recent {
		if r.WorkType != o.WorkType || (r.Status != model.OrderDone && r.Status != model.OrderPartiallyDone) {
			continue
		}
		if prev == nil || r.UpdatedAt.After(recent[*prev].UpdatedAt) ||
			(r.UpdatedAt.Equal(recent[*prev].UpdatedAt) && r.OrderID > recent[*prev].OrderID) {
			prev = &i
		}
	}
	if prev == nil {
		return none, nil
	}
	p := recent[*prev]
	fields, err := payloadDiff(p.Payload, o.Payload)
	if err != nil {
		return OrderDiff{}, fmt.Errorf("diff with order %d: %w", p.OrderID, err)
	}
	return OrderDiff{PrevOrderID: p.OrderID, PrevStatus: string(p.Status), Fields: fields}, nil
}

// payloadDiff 比较两份 payload 的顶层字段（紧凑化后逐字节比较），按字段名排序；值相同不列出。
func payloadDiff(prev, cur json.RawMessage) ([]FieldDiff, error) {
	a, err := topLevel(prev)
	if err != nil {
		return nil, fmt.Errorf("previous payload: %w", err)
	}
	b, err := topLevel(cur)
	if err != nil {
		return nil, fmt.Errorf("current payload: %w", err)
	}
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := []FieldDiff{}
	for _, k := range keys {
		pv, cv := a[k], b[k]
		if bytes.Equal(pv, cv) {
			continue
		}
		out = append(out, FieldDiff{Field: k, Prev: orNull(pv), Cur: orNull(cv)})
	}
	return out, nil
}

// topLevel 把 payload 拆成顶层字段（值紧凑化）；空 payload 为空集合。
func topLevel(raw json.RawMessage) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range m {
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return nil, err
		}
		m[k] = buf.Bytes()
	}
	return m, nil
}

func orNull(v json.RawMessage) json.RawMessage {
	if v == nil {
		return json.RawMessage("null")
	}
	return v
}

// notifyPendingTx 在 Activate 事务内写 notify.send(PENDING_APPROVAL)，dedup_key 按票去重（重放不重发）。
func (s *ApprovalService) notifyPendingTx(ctx context.Context, tx *repository.DB, t model.ApprovalTicket, now time.Time) error {
	body, err := json.Marshal(model.NotifyEnvelope{Kind: model.NotifyPendingApproval, OrderID: t.OrderID, RunID: t.RunID,
		TicketID: t.TicketID})
	if err != nil {
		return fmt.Errorf("encode notify payload: %w", err)
	}
	ref := model.OrderRef{TenantID: t.TenantID, FarmID: t.FarmID, OrderID: t.OrderID}
	if _, err := s.d.Notify.InsertTx(ctx, tx, ref, model.NotifyDedupKey(model.NotifyPendingApproval, t.OrderID, t.TicketID),
		body, now); err != nil {
		return fmt.Errorf("notify pending ticket %d: %w", t.TicketID, err)
	}
	return nil
}
