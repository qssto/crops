package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// 指标事实行查询（口径见 docs/metrics.md）：每类事实一条按 scope + 时间窗过滤的 SQL，聚合在 service.MetricsService。
// 全部 SQL 不带库名前缀，只读调用方连接的默认库（生产：agri_steward），评测库 agri_eval / agri_eval_t_{n} 读不到（验收 6）。
const (
	// 会话队列：窗口内创建的 run。
	sqlMetricRuns = `SELECT id, status, created_at FROM agent_run
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id`
	// 队列 run 的事件（只取指标用到的类型）；事件不早于 run 创建，故 run_event 按 created_at>=from 裁分区。
	sqlMetricRunEvents = `SELECT e.run_id, e.seq, e.event_type, e.payload FROM run_event e
 JOIN agent_run r ON r.id=e.run_id AND r.tenant_id=e.tenant_id AND r.farm_id=e.farm_id
 WHERE e.tenant_id=? AND e.farm_id=? AND e.created_at>=? AND r.created_at>=? AND r.created_at<?
   AND e.event_type IN ('VALIDATION_FAILED','RUN_RESUMED','RUN_COMPLETED','LLM_CALL_END') ORDER BY e.run_id, e.seq`
	sqlMetricToolCalls = `SELECT status, latency_ms, created_at FROM tool_call
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id`
	sqlMetricAdvice = `SELECT outcome, created_at FROM advice_card
 WHERE tenant_id=? AND farm_id=? AND created_at>=? AND created_at<? ORDER BY id`
	// 窗口内被人工决策的票；opened_at 取该票 APPROVAL_PENDING（PREPARING → OPEN 同事务写入）的时间。
	sqlMetricTickets = `SELECT t.id, t.status, t.decided_at,
  (SELECT MIN(e.created_at) FROM run_event e WHERE e.tenant_id=t.tenant_id AND e.farm_id=t.farm_id AND e.run_id=t.run_id
     AND e.event_type='APPROVAL_PENDING' AND CAST(JSON_UNQUOTE(JSON_EXTRACT(e.payload,'$.ticket_id')) AS UNSIGNED)=t.id) AS opened_at
 FROM approval_ticket t
 WHERE t.tenant_id=? AND t.farm_id=? AND t.ticket_kind='ORDER' AND t.status IN ('APPROVED','REJECTED')
   AND t.decided_at>=? AND t.decided_at<? ORDER BY t.id`
	// 窗口内创建的作业单；dispatched 表示曾迁移到 DISPATCHED（work_order_transition）。
	sqlMetricOrders = `SELECT o.id, o.status, o.dispatch_mode, o.created_at,
  EXISTS (SELECT 1 FROM work_order_transition w WHERE w.tenant_id=o.tenant_id AND w.farm_id=o.farm_id
     AND w.order_id=o.id AND w.to_status='DISPATCHED') AS dispatched
 FROM work_order o WHERE o.tenant_id=? AND o.farm_id=? AND o.created_at>=? AND o.created_at<? ORDER BY o.id`
)

// MetricSQL 列出指标事实查询（隔离测试扫描用）。
var MetricSQL = []string{sqlMetricRuns, sqlMetricRunEvents, sqlMetricToolCalls, sqlMetricAdvice, sqlMetricTickets, sqlMetricOrders}

// RunDetailSQL 列出 run 详情查询（隔离测试扫描用）。
var RunDetailSQL = []string{sqlDetailSteps, sqlDetailEvents, sqlDetailToolCalls, sqlDetailTickets, sqlDetailDecisions,
	sqlDetailOrders, sqlDetailAudit}

// MetricRun 是窗口内创建的一个 run。
type MetricRun struct {
	ID        uint64
	Status    model.RunStatus
	CreatedAt time.Time
}

// MetricEvent 是队列 run 的一条事件。
type MetricEvent struct {
	RunID   uint64
	Seq     int
	Type    model.EventType
	Payload json.RawMessage
}

// MetricToolCall 是窗口内的一次工具调用。
type MetricToolCall struct {
	Status    string
	LatencyMS int `gorm:"column:latency_ms"`
	CreatedAt time.Time
}

// MetricAdvice 是窗口内的一张建议卡。
type MetricAdvice struct {
	Outcome   model.Outcome
	CreatedAt time.Time
}

// MetricTicket 是窗口内被决策的一张票；OpenedAt 为 nil 表示缺 APPROVAL_PENDING 事件。
type MetricTicket struct {
	ID        uint64
	Status    model.TicketStatus
	DecidedAt time.Time
	OpenedAt  *time.Time
}

// MetricOrder 是窗口内创建的一张作业单。
type MetricOrder struct {
	ID           uint64
	Status       model.WorkOrderStatus
	DispatchMode string
	CreatedAt    time.Time
	Dispatched   bool
}

// MetricFacts 是 [From, To) 时间窗内一个农场的指标事实行。
type MetricFacts struct {
	Runs      []MetricRun
	Events    []MetricEvent
	ToolCalls []MetricToolCall
	Advice    []MetricAdvice
	Tickets   []MetricTicket
	Orders    []MetricOrder
}

// MetricsRepo 读指标事实行（只读）。
type MetricsRepo struct{ db *gorm.DB }

// NewMetricsRepo 构造 MetricsRepo；db 须是生产业务库连接。
func NewMetricsRepo(db *gorm.DB) *MetricsRepo { return &MetricsRepo{db: db} }

// Facts 读 [from, to) 内的事实行，每类一条 SQL。
func (r *MetricsRepo) Facts(ctx context.Context, scope model.Scope, from, to time.Time) (MetricFacts, error) {
	var f MetricFacts
	win := []any{scope.TenantID, scope.FarmID, from, to}
	var events []struct {
		RunID     uint64
		Seq       int
		EventType string
		Payload   []byte
	}
	var runs []struct {
		ID        uint64
		Status    string
		CreatedAt time.Time
	}
	var advice []struct {
		Outcome   string
		CreatedAt time.Time
	}
	var tickets []struct {
		ID        uint64
		Status    string
		DecidedAt time.Time
		OpenedAt  *time.Time
	}
	var orders []struct {
		ID           uint64
		Status       string
		DispatchMode string
		CreatedAt    time.Time
		Dispatched   bool
	}
	queries := []struct {
		name  string
		query string
		args  []any
		dst   any
	}{
		{"agent_run", sqlMetricRuns, win, &runs},
		{"run_event", sqlMetricRunEvents, []any{scope.TenantID, scope.FarmID, from, from, to}, &events},
		{"tool_call", sqlMetricToolCalls, win, &f.ToolCalls},
		{"advice_card", sqlMetricAdvice, win, &advice},
		{"approval_ticket", sqlMetricTickets, win, &tickets},
		{"work_order", sqlMetricOrders, win, &orders},
	}
	for _, q := range queries {
		if err := r.db.WithContext(ctx).Raw(q.query, q.args...).Scan(q.dst).Error; err != nil {
			return MetricFacts{}, fmt.Errorf("repository: metric facts %s: %w", q.name, err)
		}
	}
	for _, x := range runs {
		f.Runs = append(f.Runs, MetricRun{ID: x.ID, Status: model.RunStatus(x.Status), CreatedAt: x.CreatedAt})
	}
	for _, x := range events {
		f.Events = append(f.Events, MetricEvent{RunID: x.RunID, Seq: x.Seq, Type: model.EventType(x.EventType),
			Payload: rawJSON(x.Payload)})
	}
	for _, x := range advice {
		f.Advice = append(f.Advice, MetricAdvice{Outcome: model.Outcome(x.Outcome), CreatedAt: x.CreatedAt})
	}
	for _, x := range tickets {
		f.Tickets = append(f.Tickets, MetricTicket{ID: x.ID, Status: model.TicketStatus(x.Status),
			DecidedAt: x.DecidedAt, OpenedAt: x.OpenedAt})
	}
	for _, x := range orders {
		f.Orders = append(f.Orders, MetricOrder{ID: x.ID, Status: model.WorkOrderStatus(x.Status),
			DispatchMode: x.DispatchMode, CreatedAt: x.CreatedAt, Dispatched: x.Dispatched})
	}
	return f, nil
}
