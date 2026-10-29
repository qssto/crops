package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// run 详情的只读 SQL（P0 L7 功能任务 5）：每表按 run_id 一次查询，不随 step / 工具数增长（无 N+1）。
const (
	sqlDetailSteps = `SELECT step_id, node, status, started_at, ended_at, error_code FROM run_step
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`
	sqlDetailEvents = `SELECT seq, step_id, tool_call_id, event_type, actor, payload, created_at FROM run_event
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY seq`
	sqlDetailToolCalls = `SELECT id, step_id, attempt, epoch, tool_name, policy_level, args_digest, status, error_code, source,
  data_freshness_sec, time_sensitive, result_summary, raw_object_id, latency_ms, superseded_at, created_at FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`
	sqlDetailTickets = `SELECT ` + ticketColumns + ` FROM approval_ticket
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`
	sqlDetailDecisions = `SELECT d.id, d.ticket_id, d.decision, d.operator_id, d.operator_role, d.snapshot_digest, d.comment, d.created_at
FROM approval_decision d JOIN approval_ticket t ON t.id=d.ticket_id AND t.tenant_id=d.tenant_id AND t.farm_id=d.farm_id
WHERE t.tenant_id=? AND t.farm_id=? AND t.run_id=? ORDER BY d.id`
	sqlDetailOrders = `SELECT id, field_id, advice_id, work_type, device_type, payload, time_window_from, time_window_to, status,
  dispatch_mode, dispatch_result, done_area_mu, created_at, updated_at FROM work_order
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`
	sqlDetailAudit = `SELECT id, actor_type, actor_id, action, target, payload_digest, summary, result, raw_object_id, created_at FROM audit_log
 WHERE tenant_id=? AND farm_id=? AND run_id=? ORDER BY id`
)

// StepRow 是 run_step 一行。
type StepRow struct {
	StepID    string     `json:"step_id"`
	Node      string     `json:"node"`
	Status    string     `json:"status"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ErrorCode *string    `json:"error_code,omitempty"`
}

// EventRow 是 run_event 一行（payload 已脱敏）。
type EventRow struct {
	Seq        int             `json:"seq"`
	StepID     string          `json:"step_id,omitempty"`
	ToolCallID *uint64         `json:"tool_call_id,omitempty"`
	Type       model.EventType `json:"event_type"`
	Actor      string          `json:"actor"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ToolCallRow 是 tool_call 一行。
type ToolCallRow struct {
	ID               uint64          `json:"tool_call_id"`
	StepID           string          `json:"step_id"`
	Attempt          int             `json:"attempt"`
	Epoch            int             `json:"epoch"`
	ToolName         string          `json:"tool_name"`
	PolicyLevel      string          `json:"policy_level"`
	ArgsDigest       string          `json:"args_digest"`
	Status           string          `json:"status"`
	ErrorCode        *string         `json:"error_code,omitempty"`
	Source           *string         `json:"source,omitempty"`
	DataFreshnessSec *int            `json:"data_freshness_sec,omitempty"`
	TimeSensitive    bool            `json:"time_sensitive"`
	ResultSummary    json.RawMessage `json:"result_summary,omitempty" gorm:"-"`
	RawObjectID      *uint64         `json:"raw_object_id,omitempty"`
	LatencyMS        int             `json:"latency_ms" gorm:"column:latency_ms"`
	SupersededAt     *time.Time      `json:"superseded_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
}

// DecisionDetailRow 是 run 详情里 approval_decision 的一行（JSON 视图）。
type DecisionDetailRow struct {
	ID             uint64    `json:"decision_id"`
	TicketID       uint64    `json:"ticket_id"`
	Decision       string    `json:"decision"`
	OperatorID     uint64    `json:"operator_id"`
	OperatorRole   string    `json:"operator_role"`
	SnapshotDigest string    `json:"snapshot_digest"`
	Comment        *string   `json:"comment,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// OrderRow 是 work_order 一行。
type OrderRow struct {
	ID             uint64          `json:"order_id"`
	FieldID        uint64          `json:"field_id"`
	AdviceID       uint64          `json:"advice_id"`
	WorkType       string          `json:"work_type"`
	DeviceType     string          `json:"device_type"`
	Payload        json.RawMessage `json:"payload" gorm:"-"`
	TimeWindowFrom time.Time       `json:"time_window_from"`
	TimeWindowTo   time.Time       `json:"time_window_to"`
	Status         string          `json:"status"`
	DispatchMode   string          `json:"dispatch_mode"`
	DispatchResult json.RawMessage `json:"dispatch_result,omitempty" gorm:"-"`
	DoneAreaMu     *float64        `json:"done_area_mu,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// AuditRow 是 audit_log 一行（summary 已脱敏）。
type AuditRow struct {
	ID            uint64          `json:"audit_id"`
	ActorType     string          `json:"actor_type"`
	ActorID       uint64          `json:"actor_id"`
	Action        string          `json:"action"`
	Target        *string         `json:"target,omitempty"`
	PayloadDigest string          `json:"payload_digest"`
	Summary       json.RawMessage `json:"summary,omitempty" gorm:"-"`
	Result        string          `json:"result"`
	RawObjectID   *uint64         `json:"raw_object_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// RunDetailRows 是一个 run 在各表的原始行；组装（按 step 分组等）在 service.RunDetailService。
// 除 Run 外任何一表无行都是合法状态（run 未走到该环节），对应切片为空。
type RunDetailRows struct {
	Run         model.RunSummary
	Card        *model.AdviceCard // nil：未产出建议卡
	Steps       []StepRow
	Events      []EventRow
	ToolCalls   []ToolCallRow
	Tickets     []model.ApprovalTicket
	Decisions   []DecisionDetailRow
	Orders      []OrderRow
	Transitions []model.OrderTransition
	Audits      []AuditRow
}

// RunDetailRepo 读 run 详情（只读；db 可为生产库或 trial 库连接）。
type RunDetailRepo struct {
	db     *gorm.DB
	runs   *RunRepo
	advice *AdviceRepo
}

// NewRunDetailRepo 构造 RunDetailRepo；runs / advice 复用 L4 的 Summary 与 GetByRun。
func NewRunDetailRepo(db *gorm.DB, runs *RunRepo, advice *AdviceRepo) *RunDetailRepo {
	return &RunDetailRepo{db: db, runs: runs, advice: advice}
}

// Load 读该 run 的全部原始行；run 不存在（或不属于 scope）返回 model.ErrNotFound。
func (r *RunDetailRepo) Load(ctx context.Context, scope model.Scope, runID uint64) (RunDetailRows, error) {
	sum, err := r.runs.Summary(ctx, scope, runID)
	if err != nil {
		return RunDetailRows{}, err
	}
	out := RunDetailRows{Run: sum}
	card, err := r.advice.GetByRun(ctx, scope, runID)
	switch {
	case errors.Is(err, model.ErrNotFound):
	case err != nil:
		return RunDetailRows{}, err
	default:
		out.Card = &card
	}
	args := []any{scope.TenantID, scope.FarmID, runID}
	loaders := []struct {
		name string
		load func() error
	}{
		{"run_step", func() error { return r.scan(ctx, sqlDetailSteps, args, &out.Steps) }},
		{"run_event", func() (err error) { out.Events, err = r.events(ctx, args); return err }},
		{"tool_call", func() (err error) { out.ToolCalls, err = r.toolCalls(ctx, args); return err }},
		{"approval_ticket", func() (err error) { out.Tickets, err = r.tickets(ctx, args); return err }},
		{"approval_decision", func() error { return r.scan(ctx, sqlDetailDecisions, args, &out.Decisions) }},
		{"work_order", func() (err error) { out.Orders, err = r.orders(ctx, args); return err }},
		{"work_order_transition", func() (err error) {
			out.Transitions, err = WorkOrderRepo{}.TransitionsByRun(ctx, r.db, scope, runID)
			return err
		}},
		{"audit_log", func() (err error) { out.Audits, err = r.audits(ctx, args); return err }},
	}
	for _, l := range loaders {
		if err := l.load(); err != nil {
			return RunDetailRows{}, fmt.Errorf("repository: run %d detail %s: %w", runID, l.name, err)
		}
	}
	return out, nil
}

func (r *RunDetailRepo) scan(ctx context.Context, query string, args []any, dst any) error {
	return r.db.WithContext(ctx).Raw(query, args...).Scan(dst).Error
}

// rawJSON 把 json 列（驱动返回 []byte）转成 RawMessage；NULL 保持 nil。
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (r *RunDetailRepo) events(ctx context.Context, args []any) ([]EventRow, error) {
	var rows []struct {
		Seq        int
		StepID     *string
		ToolCallID *uint64
		EventType  string
		Actor      string
		Payload    []byte
		CreatedAt  time.Time
	}
	if err := r.scan(ctx, sqlDetailEvents, args, &rows); err != nil {
		return nil, err
	}
	out := make([]EventRow, 0, len(rows))
	for _, e := range rows {
		out = append(out, EventRow{Seq: e.Seq, StepID: deref(e.StepID), ToolCallID: e.ToolCallID,
			Type: model.EventType(e.EventType), Actor: e.Actor, Payload: rawJSON(e.Payload), CreatedAt: e.CreatedAt})
	}
	return out, nil
}

func (r *RunDetailRepo) toolCalls(ctx context.Context, args []any) ([]ToolCallRow, error) {
	var rows []struct {
		ToolCallRow
		ResultSummaryRaw []byte `gorm:"column:result_summary"`
	}
	if err := r.scan(ctx, sqlDetailToolCalls, args, &rows); err != nil {
		return nil, err
	}
	out := make([]ToolCallRow, 0, len(rows))
	for _, c := range rows {
		row := c.ToolCallRow
		row.ResultSummary = rawJSON(c.ResultSummaryRaw)
		out = append(out, row)
	}
	return out, nil
}

func (r *RunDetailRepo) tickets(ctx context.Context, args []any) ([]model.ApprovalTicket, error) {
	var rows []ticketRow
	if err := r.scan(ctx, sqlDetailTickets, args, &rows); err != nil {
		return nil, err
	}
	out := make([]model.ApprovalTicket, 0, len(rows))
	for _, row := range rows {
		t, err := row.ticket()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *RunDetailRepo) orders(ctx context.Context, args []any) ([]OrderRow, error) {
	var rows []struct {
		OrderRow
		PayloadRaw        []byte `gorm:"column:payload"`
		DispatchResultRaw []byte `gorm:"column:dispatch_result"`
	}
	if err := r.scan(ctx, sqlDetailOrders, args, &rows); err != nil {
		return nil, err
	}
	out := make([]OrderRow, 0, len(rows))
	for _, o := range rows {
		row := o.OrderRow
		row.Payload, row.DispatchResult = rawJSON(o.PayloadRaw), rawJSON(o.DispatchResultRaw)
		out = append(out, row)
	}
	return out, nil
}

func (r *RunDetailRepo) audits(ctx context.Context, args []any) ([]AuditRow, error) {
	var rows []struct {
		AuditRow
		SummaryRaw []byte `gorm:"column:summary"`
	}
	if err := r.scan(ctx, sqlDetailAudit, args, &rows); err != nil {
		return nil, err
	}
	out := make([]AuditRow, 0, len(rows))
	for _, a := range rows {
		row := a.AuditRow
		row.Summary = rawJSON(a.SummaryRaw)
		out = append(out, row)
	}
	return out, nil
}
