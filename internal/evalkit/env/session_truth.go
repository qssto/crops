package env

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// SessionTruth 是 compaction_retained 的真值：按 SeedSession 返回的种子 run_id 逐表独立查询 trial 库，
// 不复用生产 RunRepo.LoadSession（打分真值与被测读法分开，避免同一处错误两边一致而空过）。
//   - Source：FieldID 取本次 run；Turns / Cards / Orders / Measured / CitedKBIDs 取种子链各 run；
//   - Linked：本次 run 的 prev_run_id 是种子链末轮（开关开时续接成功）；
//   - Superseded：种子链上已被取代的时效实测 tool_call.id（不得出现在会话块 facts 中）。
type SessionTruth struct {
	Source     model.SessionSource
	Linked     bool
	Superseded []uint64
}

// ReadSessionTruth 读种子会话链的真值；seeded 为由早到晚的种子 run_id，runID 为本次提问的 run。
func ReadSessionTruth(ctx context.Context, db *repository.DB, scope model.Scope, seeded []uint64, runID uint64) (SessionTruth, error) {
	if db == nil || len(seeded) == 0 || runID == 0 {
		return SessionTruth{}, fmt.Errorf("%w: db, seeded runs and run id are required", errSessionSeed)
	}
	q := truthQuery{db: db.WithContext(ctx), scope: scope}
	var cur struct {
		FieldID   uint64
		PrevRunID *uint64
	}
	res := q.db.Raw(`SELECT field_id, prev_run_id FROM agent_run WHERE tenant_id = ? AND farm_id = ? AND id = ?`,
		scope.TenantID, scope.FarmID, runID).Scan(&cur)
	if res.Error != nil {
		return SessionTruth{}, fmt.Errorf("env: truth run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return SessionTruth{}, fmt.Errorf("env: truth run %d: %w", runID, model.ErrNotFound)
	}
	t := SessionTruth{Source: model.SessionSource{RunID: runID, FieldID: cur.FieldID},
		Linked: cur.PrevRunID != nil && *cur.PrevRunID == seeded[len(seeded)-1]}
	orders, err := q.workOrders(seeded)
	if err != nil {
		return SessionTruth{}, err
	}
	if t.Source.Cards, err = q.cards(seeded, orders); err != nil {
		return SessionTruth{}, err
	}
	if t.Source.Turns, err = q.turns(seeded, t.Source.Cards); err != nil {
		return SessionTruth{}, err
	}
	if t.Source.Orders, err = q.orders(orders, t.Source.Cards); err != nil {
		return SessionTruth{}, err
	}
	if t.Source.Measured, t.Superseded, err = q.measured(seeded); err != nil {
		return SessionTruth{}, err
	}
	if t.Source.CitedKBIDs, err = q.kbIDs(t.Source.Cards); err != nil {
		return SessionTruth{}, err
	}
	return t, nil
}

// truthQuery 是真值读取的单表查询（全部带 tenant_id / farm_id）。
type truthQuery struct {
	db    *repository.DB
	scope model.Scope
}

// turns：每个种子 run 一轮，问题取该 run 第一条 RUN_STARTED，回答取该 run 建议卡结论。
func (q truthQuery) turns(runs []uint64, cards []model.SessionCard) ([]model.SessionTurn, error) {
	var rows []struct {
		RunID   uint64
		Payload []byte
	}
	if err := q.db.Raw(`SELECT run_id, payload FROM run_event WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?)
  AND event_type = ? ORDER BY run_id, seq`, q.scope.TenantID, q.scope.FarmID, runs, string(model.EventRunStarted)).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("env: truth questions: %w", err)
	}
	questions := map[uint64]string{}
	for _, row := range rows {
		if _, ok := questions[row.RunID]; ok {
			continue
		}
		var p struct {
			Question string `json:"question"`
		}
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			return nil, fmt.Errorf("env: truth RUN_STARTED run %d: %w", row.RunID, err)
		}
		questions[row.RunID] = p.Question
	}
	answers := map[uint64]string{}
	for _, c := range cards {
		answers[c.RunID] = c.Conclusion
	}
	out := make([]model.SessionTurn, 0, len(runs))
	for _, id := range runs {
		out = append(out, model.SessionTurn{RunID: id, Question: questions[id], Answer: answers[id]})
	}
	return out, nil
}

// truthOrder 是一行 work_order。
type truthOrder struct {
	ID             uint64
	AdviceID       uint64
	RunID          uint64
	Status         string
	TimeWindowFrom *time.Time
	TimeWindowTo   *time.Time
}

func (q truthQuery) workOrders(runs []uint64) ([]truthOrder, error) {
	var rows []truthOrder
	if err := q.db.Raw(`SELECT id, advice_id, run_id, status, time_window_from, time_window_to FROM work_order
  WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) ORDER BY id`, q.scope.TenantID, q.scope.FarmID, runs).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("env: truth work orders: %w", err)
	}
	return rows, nil
}

// cards：种子链上的建议卡（按 id 序）；作业窗口取 advice_id 对应的作业单。
func (q truthQuery) cards(runs []uint64, orders []truthOrder) ([]model.SessionCard, error) {
	var rows []struct {
		ID              uint64
		RunID           uint64
		Crop            string
		PhenologyStage  string
		Outcome         string
		Conclusion      string
		Numbers         []byte
		SelectedProduct *string
	}
	if err := q.db.Raw(`SELECT id, run_id, crop, phenology_stage, outcome, conclusion, numbers, selected_product
  FROM advice_card WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) ORDER BY id`,
		q.scope.TenantID, q.scope.FarmID, runs).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("env: truth advice cards: %w", err)
	}
	out := make([]model.SessionCard, 0, len(rows))
	for _, r := range rows {
		c := model.SessionCard{AdviceID: r.ID, RunID: r.RunID, Crop: r.Crop, PhenologyStage: r.PhenologyStage,
			Outcome: model.Outcome(r.Outcome), Conclusion: r.Conclusion}
		if r.SelectedProduct != nil {
			c.SelectedProduct = *r.SelectedProduct
		}
		if len(r.Numbers) > 0 {
			if err := json.Unmarshal(r.Numbers, &c.Numbers); err != nil {
				return nil, fmt.Errorf("env: truth advice card %d numbers: %w", r.ID, err)
			}
		}
		for _, o := range orders {
			if o.AdviceID == r.ID && o.TimeWindowFrom != nil && o.TimeWindowTo != nil {
				c.Window = &model.TimeWindow{From: o.TimeWindowFrom.UTC(), To: o.TimeWindowTo.UTC()}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// orders：作业单 + 最新一张 ORDER 票（ticket_seq 最大）+ 该票驳回原因 + 最后一条进入 CANCELED 的迁移原因，
// 药剂取所出自建议卡的 selected_product。各表单独查询，在 Go 里拼接。
func (q truthQuery) orders(orders []truthOrder, cards []model.SessionCard) ([]model.SessionOrderSource, error) {
	if len(orders) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(orders))
	for _, o := range orders {
		ids = append(ids, o.ID)
	}
	var tickets []struct {
		ID        uint64
		OrderID   uint64
		TicketSeq int
		Status    string
	}
	if err := q.db.Raw(`SELECT id, order_id, ticket_seq, status FROM approval_ticket
  WHERE tenant_id = ? AND farm_id = ? AND ticket_kind = 'ORDER' AND order_id IN (?)`,
		q.scope.TenantID, q.scope.FarmID, ids).Scan(&tickets).Error; err != nil {
		return nil, fmt.Errorf("env: truth tickets: %w", err)
	}
	var rejects []struct {
		TicketID   uint64
		ReasonCode string
		ReasonText *string
	}
	if err := q.db.Raw(`SELECT ticket_id, reason_code, reason_text FROM rejection_reason
  WHERE tenant_id = ? AND farm_id = ? AND order_id IN (?)`, q.scope.TenantID, q.scope.FarmID, ids).
		Scan(&rejects).Error; err != nil {
		return nil, fmt.Errorf("env: truth rejection reasons: %w", err)
	}
	var cancels []struct {
		OrderID    uint64
		ReasonCode *string
	}
	if err := q.db.Raw(`SELECT order_id, reason_code FROM work_order_transition
  WHERE tenant_id = ? AND farm_id = ? AND order_id IN (?) AND to_status = ? ORDER BY id`,
		q.scope.TenantID, q.scope.FarmID, ids, string(model.OrderCanceled)).Scan(&cancels).Error; err != nil {
		return nil, fmt.Errorf("env: truth cancel transitions: %w", err)
	}
	out := make([]model.SessionOrderSource, 0, len(orders))
	for _, o := range orders {
		s := model.SessionOrderSource{RunID: o.RunID,
			SessionOrder: model.SessionOrder{OrderID: o.ID, AdviceID: o.AdviceID, Status: model.WorkOrderStatus(o.Status)}}
		seq := -1
		for _, t := range tickets {
			if t.OrderID == o.ID && t.TicketSeq > seq {
				seq, s.TicketID, s.TicketStatus = t.TicketSeq, t.ID, model.TicketStatus(t.Status)
			}
		}
		for _, r := range rejects {
			if s.TicketID != 0 && r.TicketID == s.TicketID {
				s.RejectReasonCode = r.ReasonCode
				if r.ReasonText != nil {
					s.RejectText = *r.ReasonText
				}
			}
		}
		for _, c := range cancels { // 按 id 升序，最后一条为准
			if c.OrderID == o.ID && c.ReasonCode != nil {
				s.CancelReason = model.ReasonCode(*c.ReasonCode)
			}
		}
		for _, c := range cards {
			if c.AdviceID == o.AdviceID {
				s.Product = c.SelectedProduct
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// measured：种子链上的时效工具结果，未被取代的进 Measured，已取代的只记 id。
func (q truthQuery) measured(runs []uint64) ([]model.MeasuredFact, []uint64, error) {
	var rows []struct {
		ID            uint64
		ToolName      string
		ResultSummary []byte
		CreatedAt     time.Time
		SupersededAt  *time.Time
	}
	if err := q.db.Raw(`SELECT id, tool_name, result_summary, created_at, superseded_at FROM tool_call
  WHERE tenant_id = ? AND farm_id = ? AND run_id IN (?) AND time_sensitive = 1 ORDER BY id`,
		q.scope.TenantID, q.scope.FarmID, runs).Scan(&rows).Error; err != nil {
		return nil, nil, fmt.Errorf("env: truth tool calls: %w", err)
	}
	var live []model.MeasuredFact
	var superseded []uint64
	for _, r := range rows {
		if r.SupersededAt != nil {
			superseded = append(superseded, r.ID)
			continue
		}
		summary := json.RawMessage(r.ResultSummary)
		if len(summary) == 0 {
			summary = json.RawMessage("null")
		}
		live = append(live, model.MeasuredFact{ToolCallID: r.ID, ToolName: r.ToolName, ResultSummary: summary,
			CollectedAt: r.CreatedAt.UTC()})
	}
	return live, superseded, nil
}

// kbIDs：种子建议卡的 kb 引用（type=kb，按 id 序，不去重）。
func (q truthQuery) kbIDs(cards []model.SessionCard) ([]string, error) {
	if len(cards) == 0 {
		return nil, nil
	}
	ids := make([]uint64, 0, len(cards))
	for _, c := range cards {
		ids = append(ids, c.AdviceID)
	}
	var out []string
	if err := q.db.Raw(`SELECT kb_id FROM advice_citation WHERE tenant_id = ? AND farm_id = ? AND advice_id IN (?)
  AND type = ? AND kb_id IS NOT NULL ORDER BY id`, q.scope.TenantID, q.scope.FarmID, ids, string(model.CitationKB)).
		Scan(&out).Error; err != nil {
		return nil, fmt.Errorf("env: truth kb citations: %w", err)
	}
	return out, nil
}
