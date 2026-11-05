package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"agri-steward/internal/model"
)

// LoadSession 读一次上下文组装的会话原料（实现 compaction.Source；断言时再调一次即「从源表重新读取」）：
// 沿 prev_run_id 由早到晚取会话链（最多 sessionChainMax 个 run，含本 run），
// Turns 取链上本 run 之前各 run 的 RUN_STARTED 问题（已脱敏）与建议卡结论，其余业务行取整条链。
// 链被 sessionChainMax 截断时（链首仍有 prev_run_id），另按同地块同用户汇总更早 run 的问题与作业单（见 SessionSource）。
// 本 run 不在 Scope 内返回 model.ErrNotFound。
func (r *RunRepo) LoadSession(ctx context.Context, scope model.Scope, runID uint64) (model.SessionSource, error) {
	if err := requireScope(scope); err != nil {
		return model.SessionSource{}, err
	}
	var cur struct {
		FieldID         uint64
		UserID          uint64
		CompactionState []byte
	}
	res := r.db.WithContext(ctx).Raw(
		`SELECT field_id, user_id, compaction_state FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		scope.TenantID, scope.FarmID, runID).Scan(&cur)
	if res.Error != nil {
		return model.SessionSource{}, fmt.Errorf("repository: get agent_run %d session: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.SessionSource{}, fmt.Errorf("repository: agent_run %d: %w", runID, model.ErrNotFound)
	}
	src := model.SessionSource{RunID: runID, FieldID: cur.FieldID}
	if cur.CompactionState != nil {
		src.State = json.RawMessage(cur.CompactionState)
	}
	chain, truncated, err := r.sessionChain(ctx, scope, runID)
	if err != nil {
		return model.SessionSource{}, err
	}
	if truncated {
		if err := r.sessionEarlier(ctx, scope, cur.FieldID, cur.UserID, chain[0], &src); err != nil {
			return model.SessionSource{}, err
		}
	}
	if src.Cards, err = r.sessionCards(ctx, scope, chain); err != nil {
		return model.SessionSource{}, err
	}
	if src.Turns, err = r.sessionTurns(ctx, scope, chain[:len(chain)-1], src.Cards); err != nil {
		return model.SessionSource{}, err
	}
	if src.Orders, err = r.sessionOrders(ctx, scope, chain); err != nil {
		return model.SessionSource{}, err
	}
	if src.Measured, err = r.sessionMeasured(ctx, scope, chain); err != nil {
		return model.SessionSource{}, err
	}
	if src.CitedKBIDs, err = r.sessionKBIDs(ctx, scope, chain); err != nil {
		return model.SessionSource{}, err
	}
	return src, nil
}

// sessionChain 返回由早到晚的会话链 run_id，末项为 runID（文档未给 SQL；递归 CTE 一次取完，深度受限）；
// truncated 表示链达到 sessionChainMax 且链首仍有 prev_run_id（更早的 run 被截掉）。
func (r *RunRepo) sessionChain(ctx context.Context, scope model.Scope, runID uint64) ([]uint64, bool, error) {
	var rows []struct {
		ID        uint64
		PrevRunID *uint64
	}
	err := r.db.WithContext(ctx).Raw(
		`WITH RECURSIVE chain (id, prev_run_id, depth) AS (
  SELECT id, prev_run_id, 0 FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ?
  UNION ALL
  SELECT r.id, r.prev_run_id, c.depth + 1 FROM agent_run r JOIN chain c ON r.id = c.prev_run_id
  WHERE r.tenant_id = ? AND r.farm_id = ? AND c.depth + 1 < ?)
SELECT id, prev_run_id FROM chain ORDER BY depth DESC`,
		scope.TenantID, scope.FarmID, runID, scope.TenantID, scope.FarmID, sessionChainMax).Scan(&rows).Error
	if err != nil {
		return nil, false, fmt.Errorf("repository: session chain run %d: %w", runID, err)
	}
	if len(rows) == 0 || rows[len(rows)-1].ID != runID {
		return nil, false, fmt.Errorf("repository: session chain run %d: %w", runID, model.ErrNotFound)
	}
	ids := make([]uint64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	truncated := len(rows) == sessionChainMax && rows[0].PrevRunID != nil && *rows[0].PrevRunID != 0
	return ids, truncated, nil
}

// sessionEarlier 汇总被截掉的更早 run：同租户 / 农场 / 地块 / 用户且 id 早于链首（走 idx_user，不依赖链深）。
// 只取问题（用户约束）与作业单（否决药剂），不进轮次原文与结论。
func (r *RunRepo) sessionEarlier(ctx context.Context, scope model.Scope, fieldID, userID, head uint64, src *model.SessionSource) error {
	var ids []uint64
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND user_id = ? AND field_id = ? AND id < ? ORDER BY id`,
		scope.TenantID, scope.FarmID, userID, fieldID, head).Scan(&ids).Error; err != nil {
		return fmt.Errorf("repository: session earlier runs before %d: %w", head, err)
	}
	src.Truncated, src.EarlierRuns = true, len(ids)
	if len(ids) == 0 {
		return nil
	}
	var err error
	if src.EarlierTurns, err = r.sessionTurns(ctx, scope, ids, nil); err != nil {
		return err
	}
	if src.EarlierOrders, err = r.sessionOrders(ctx, scope, ids); err != nil {
		return err
	}
	return nil
}

// sessionTurns：每个 run 一轮，问题取 RUN_STARTED（经 pii.Mask），回答取该 run 建议卡结论（无事件 / 无卡为空）。
func (r *RunRepo) sessionTurns(ctx context.Context, scope model.Scope, runs []uint64, cards []model.SessionCard) ([]model.SessionTurn, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	var rows []struct {
		RunID   uint64
		Payload []byte
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT run_id, payload FROM run_event WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) AND event_type = ? ORDER BY run_id, seq`,
		scope.TenantID, scope.FarmID, runs, string(model.EventRunStarted)).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: session questions: %w", err)
	}
	questions := make(map[uint64]string, len(rows))
	for _, row := range rows {
		if _, seen := questions[row.RunID]; seen {
			continue
		}
		var p model.RunStartedPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return nil, fmt.Errorf("repository: decode RUN_STARTED run %d: %w", row.RunID, err)
		}
		questions[row.RunID] = p.Question
	}
	turns := make([]model.SessionTurn, 0, len(runs))
	for _, id := range runs {
		t := model.SessionTurn{RunID: id, Question: questions[id]}
		if i := slices.IndexFunc(cards, func(c model.SessionCard) bool { return c.RunID == id }); i >= 0 {
			t.Answer = cards[i].Conclusion
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// sessionCards：会话链上的建议卡（按 id 序），作业窗口取对应作业单（无单为 nil）。
func (r *RunRepo) sessionCards(ctx context.Context, scope model.Scope, runs []uint64) ([]model.SessionCard, error) {
	var rows []struct {
		ID              uint64
		RunID           uint64
		Crop            string
		PhenologyStage  string
		Outcome         string
		Conclusion      string
		Numbers         []byte
		SelectedProduct *string
		WindowFrom      *time.Time
		WindowTo        *time.Time
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT c.id, c.run_id, c.crop, c.phenology_stage, c.outcome, c.conclusion, c.numbers, c.selected_product,
  o.time_window_from AS window_from, o.time_window_to AS window_to
FROM advice_card c LEFT JOIN work_order o ON o.tenant_id = c.tenant_id AND o.farm_id = c.farm_id AND o.advice_id = c.id
WHERE c.tenant_id = ? AND c.farm_id = ? AND c.run_id IN (?) ORDER BY c.id`,
		scope.TenantID, scope.FarmID, runs).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: session cards: %w", err)
	}
	cards := make([]model.SessionCard, 0, len(rows))
	for _, row := range rows {
		c := model.SessionCard{AdviceID: row.ID, RunID: row.RunID, Crop: row.Crop, PhenologyStage: row.PhenologyStage,
			Outcome: model.Outcome(row.Outcome), Conclusion: row.Conclusion}
		if row.SelectedProduct != nil {
			c.SelectedProduct = *row.SelectedProduct
		}
		if len(row.Numbers) > 0 {
			if err := json.Unmarshal(row.Numbers, &c.Numbers); err != nil {
				return nil, fmt.Errorf("repository: decode advice_card %d numbers: %w", row.ID, err)
			}
		}
		if row.WindowFrom != nil && row.WindowTo != nil {
			c.Window = &model.TimeWindow{From: row.WindowFrom.UTC(), To: row.WindowTo.UTC()}
		}
		cards = append(cards, c)
	}
	return cards, nil
}

// sessionOrders：会话链上的作业单（含所出自的建议卡 advice_id）、最新一张 ORDER 票（ticket_seq 最大）及其驳回原因；
// Product 取对应建议卡所选药剂；CancelReason 取进入 CANCELED 的那条迁移的 reason_code（用户否决判定用，只追加表取最新一条）。
func (r *RunRepo) sessionOrders(ctx context.Context, scope model.Scope, runs []uint64) ([]model.SessionOrderSource, error) {
	var rows []struct {
		OrderID      uint64
		AdviceID     uint64
		RunID        uint64
		Status       string
		TicketID     *uint64
		TicketStatus *string
		ReasonCode   *string
		ReasonText   *string
		Product      *string
		CancelReason *string
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT o.id AS order_id, o.advice_id, o.run_id, o.status, t.id AS ticket_id, t.status AS ticket_status,
  rr.reason_code, rr.reason_text, c.selected_product AS product,
  (SELECT tr.reason_code FROM work_order_transition tr
    WHERE tr.tenant_id = ? AND tr.farm_id = ? AND tr.order_id = o.id AND tr.to_status = 'CANCELED'
    ORDER BY tr.id DESC LIMIT 1) AS cancel_reason
FROM work_order o
LEFT JOIN approval_ticket t ON t.tenant_id = o.tenant_id AND t.farm_id = o.farm_id AND t.ticket_kind = 'ORDER' AND t.order_id = o.id
  AND t.ticket_seq = (SELECT MAX(t2.ticket_seq) FROM approval_ticket t2
    WHERE t2.tenant_id = ? AND t2.farm_id = ? AND t2.ticket_kind = 'ORDER' AND t2.order_id = o.id)
LEFT JOIN rejection_reason rr ON rr.tenant_id = o.tenant_id AND rr.farm_id = o.farm_id AND rr.ticket_id = t.id
LEFT JOIN advice_card c ON c.tenant_id = o.tenant_id AND c.farm_id = o.farm_id AND c.id = o.advice_id
WHERE o.tenant_id = ? AND o.farm_id = ? AND o.run_id IN (?) ORDER BY o.id`,
		scope.TenantID, scope.FarmID, scope.TenantID, scope.FarmID, scope.TenantID, scope.FarmID, runs).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: session orders: %w", err)
	}
	out := make([]model.SessionOrderSource, 0, len(rows))
	for _, row := range rows {
		o := model.SessionOrderSource{RunID: row.RunID,
			SessionOrder: model.SessionOrder{OrderID: row.OrderID, AdviceID: row.AdviceID, Status: model.WorkOrderStatus(row.Status)},
			CancelReason: model.ReasonCode(orZero(row.CancelReason))}
		o.TicketID, o.Product = orZero(row.TicketID), orZero(row.Product)
		o.TicketStatus = model.TicketStatus(orZero(row.TicketStatus))
		o.RejectReasonCode, o.RejectText = orZero(row.ReasonCode), orZero(row.ReasonText)
		out = append(out, o)
	}
	return out, nil
}

// orZero 取可空列的值，NULL 为零值。
func orZero[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// sessionMeasured 照抄时效实测取数 SQL（未被取代的 time_sensitive 工具结果）；按 id 排序保证两次读取顺序一致。
func (r *RunRepo) sessionMeasured(ctx context.Context, scope model.Scope, runs []uint64) ([]model.MeasuredFact, error) {
	var rows []struct {
		ID            uint64
		ToolName      string
		ResultSummary []byte
		CreatedAt     time.Time
	}
	if err := r.db.WithContext(ctx).Raw(
		`SELECT id, tool_name, result_summary, created_at FROM tool_call
WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) AND time_sensitive = 1 AND superseded_at IS NULL ORDER BY id`,
		scope.TenantID, scope.FarmID, runs).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: session measured: %w", err)
	}
	out := make([]model.MeasuredFact, 0, len(rows))
	for _, row := range rows {
		summary := json.RawMessage(row.ResultSummary)
		if len(summary) == 0 {
			summary = json.RawMessage("null")
		}
		out = append(out, model.MeasuredFact{ToolCallID: row.ID, ToolName: row.ToolName, ResultSummary: summary,
			CollectedAt: row.CreatedAt.UTC()})
	}
	return out, nil
}

// sessionKBIDs：会话链上建议卡引用的 kb_id（type=kb；去重排序由 compaction 统一做）。
func (r *RunRepo) sessionKBIDs(ctx context.Context, scope model.Scope, runs []uint64) ([]string, error) {
	var ids []string
	if err := r.db.WithContext(ctx).Raw(
		`SELECT ci.kb_id FROM advice_citation ci
JOIN advice_card c ON c.tenant_id = ci.tenant_id AND c.farm_id = ci.farm_id AND c.id = ci.advice_id
WHERE ci.tenant_id = ? AND ci.farm_id = ? AND c.run_id IN (?) AND ci.type = 'kb' AND ci.kb_id IS NOT NULL ORDER BY ci.id`,
		scope.TenantID, scope.FarmID, runs).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: session kb ids: %w", err)
	}
	return ids, nil
}
