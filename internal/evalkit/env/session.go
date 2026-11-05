package env

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/agent/graph"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// SessionSeed 是写入 trial 库的多轮会话（P2 L6 fixtures.session）。
type SessionSeed struct {
	Scope        model.Scope // 本次提问的租户 / 农场 / 用户（会话链须同用户）
	FieldID      uint64
	Crop         string // 建议卡 crop（样本 crop）
	KnowledgeRef string
	Now          time.Time // trial 冻结时钟；最后一轮在 Now-1m，落在 compaction.session_idle 内
	Fixture      *sample.SessionFixture
}

// errSessionSeed 表示会话种子参数不完整。
var errSessionSeed = errors.New("env: session seed")

// sessionChannel 是种子轮次 RUN_STARTED 的 channel。
const sessionChannel = "web"

// SeedSession 在一个事务里把各轮写成已结束（COMPLETED）的 run，沿 prev_run_id 由早到晚串成会话链：
// 每轮一条 RUN_STARTED（问题原文；种子样本不含 PII），声明 conclusion 的轮次再写一张 ADVICE 建议卡与其 kb 引用，
// 声明 order 的再写终态作业单（票 / 驳回原因 / 取消迁移），声明 measured 的写时效工具结果。
// 第 i 轮（从 1 计）的时间为 Now - (len-i+1) 分钟。返回各轮 run_id（由早到晚，末项为本次提问的 prev_run_id）。
func SeedSession(ctx context.Context, db *repository.DB, s SessionSeed) ([]uint64, error) {
	if db == nil || s.Fixture == nil || len(s.Fixture.Turns) == 0 || s.FieldID == 0 || s.Scope.TenantID == 0 ||
		s.Scope.FarmID == 0 || s.Scope.UserID == 0 || s.Now.IsZero() {
		return nil, fmt.Errorf("%w: db, scope, field, now and turns are required", errSessionSeed)
	}
	var runs []uint64
	err := db.WithContext(ctx).Transaction(func(tx *repository.DB) error {
		var prev any // 首轮 prev_run_id 为 NULL
		n := len(s.Fixture.Turns)
		for i, t := range s.Fixture.Turns {
			at := s.Now.Add(-time.Duration(n-i) * time.Minute)
			id, err := seedTurn(tx, s, t, prev, at)
			if err != nil {
				return fmt.Errorf("turn %d: %w", i, err)
			}
			prev, runs = id, append(runs, id)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("env: seed session: %w", err)
	}
	return runs, nil
}

// seedTurn 写一轮：agent_run → RUN_STARTED → 时效实测 →（有结论时）advice_card + advice_citation →（有作业单时）
// work_order + approval_ticket + rejection_reason / work_order_transition。返回 run_id。
func seedTurn(tx *repository.DB, s SessionSeed, t sample.SessionTurn, prev any, at time.Time) (uint64, error) {
	sc := s.Scope
	if err := tx.Exec(`INSERT INTO agent_run (tenant_id, farm_id, field_id, user_id, status, graph_version, prompt_version,
  model_tier, knowledge_ref, prev_run_id, event_seq, created_at, updated_at)
  VALUES (?, ?, ?, ?, ?, ?, 'v1', 'standard', ?, ?, 1, ?, ?)`,
		sc.TenantID, sc.FarmID, s.FieldID, sc.UserID, string(model.RunCompleted), graph.GraphVersion, s.KnowledgeRef,
		prev, at, at).Error; err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	runID, err := lastInsertID(tx)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(model.RunStartedPayload{FieldID: s.FieldID, Question: t.Question, Channel: sessionChannel})
	if err != nil {
		return 0, fmt.Errorf("encode RUN_STARTED: %w", err)
	}
	if err := tx.Exec(`INSERT INTO run_event (tenant_id, farm_id, run_id, seq, event_type, actor, payload, created_at)
  VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, sc.TenantID, sc.FarmID, runID, string(model.EventRunStarted), string(model.ActorUser),
		string(payload), at).Error; err != nil {
		return 0, fmt.Errorf("insert RUN_STARTED: %w", err)
	}
	for i, m := range t.Measured {
		if err := seedMeasured(tx, s, runID, i, m, at); err != nil {
			return 0, fmt.Errorf("measured %d: %w", i, err)
		}
	}
	if t.Conclusion == "" {
		return runID, nil
	}
	adviceID, err := seedCard(tx, s, t, runID, at)
	if err != nil {
		return 0, err
	}
	if t.Order == nil {
		return runID, nil
	}
	return runID, seedOrder(tx, s, *t.Order, runID, adviceID, at)
}

// seedCard 写该轮的 ADVICE 建议卡与 kb 引用（seq 从 1 起），返回 advice_id。
func seedCard(tx *repository.DB, s SessionSeed, t sample.SessionTurn, runID uint64, at time.Time) (uint64, error) {
	sc := s.Scope
	var product any
	if t.Product != "" {
		product = t.Product
	}
	if err := tx.Exec(`INSERT INTO advice_card (tenant_id, farm_id, field_id, run_id, crop, phenology_stage, category, advice_type,
  outcome, conclusion, reasons, selected_product, next_action, schema_ver, created_at)
  VALUES (?, ?, ?, ?, ?, ?, 'protect', '观察类', ?, ?, '[]', ?, 'NONE', 1, ?)`,
		sc.TenantID, sc.FarmID, s.FieldID, runID, s.Crop, s.Fixture.PhenologyStage, string(model.OutcomeAdvice),
		t.Conclusion, product, at).Error; err != nil {
		return 0, fmt.Errorf("insert advice card: %w", err)
	}
	adviceID, err := lastInsertID(tx)
	if err != nil {
		return 0, err
	}
	for i, ref := range t.Cite {
		id, ver, ok := sample.SplitCite(ref)
		if !ok {
			return 0, fmt.Errorf("%w: cite %q is not kb_id@version", errSessionSeed, ref)
		}
		if err := tx.Exec(`INSERT INTO advice_citation (tenant_id, farm_id, advice_id, seq, claim_path, type, kb_id, kb_version)
  VALUES (?, ?, ?, ?, 'conclusion', ?, ?, ?)`, sc.TenantID, sc.FarmID, adviceID, i+1, string(model.CitationKB), id, ver).Error; err != nil {
			return 0, fmt.Errorf("insert citation %d: %w", i, err)
		}
	}
	return adviceID, nil
}

// 种子作业单 / 票据的固定取值（只播种终态，不进派发与审批链路；审批人为虚构 operator）。
const (
	seedOperatorID   = 2001
	seedOperatorRole = "agronomist"
	seedDigest       = "0000000000000000000000000000000000000000000000000000000000000000"
)

// seedOrder 写建议卡的终态作业单：REJECTED 带一张已驳回的 ORDER 票与驳回原因；CANCELED 带一张已通过的票
// 与 APPROVED → CANCELED 迁移（reason_code 为 o.CancelReason）。
func seedOrder(tx *repository.DB, s SessionSeed, o sample.SessionOrder, runID, adviceID uint64, at time.Time) error {
	sc := s.Scope
	if err := tx.Exec(`INSERT INTO work_order (tenant_id, farm_id, field_id, advice_id, run_id, work_type, device_type, payload,
  time_window_from, time_window_to, status, dispatch_mode, schema_ver, created_at, updated_at)
  VALUES (?, ?, ?, ?, ?, 'SPRAY', 'BACKPACK', '{}', ?, ?, ?, 'DRY_RUN', 1, ?, ?)`,
		sc.TenantID, sc.FarmID, s.FieldID, adviceID, runID, at, at.Add(time.Hour), o.Status, at, at).Error; err != nil {
		return fmt.Errorf("insert work order: %w", err)
	}
	orderID, err := lastInsertID(tx)
	if err != nil {
		return err
	}
	ticket := string(model.TicketApproved)
	if o.Status == string(model.OrderRejected) {
		ticket = string(model.TicketRejected)
	}
	if err := tx.Exec(`INSERT INTO approval_ticket (tenant_id, farm_id, run_id, order_id, ticket_kind, ticket_seq, risk_level,
  required_role, initiator_id, snapshot_digest, snapshot_raw_id, status, operator_id, decided_at, created_at, updated_at)
  VALUES (?, ?, ?, ?, 'ORDER', 1, 'LOW', '["agronomist"]', ?, ?, 0, ?, ?, ?, ?, ?)`,
		sc.TenantID, sc.FarmID, runID, orderID, sc.UserID, seedDigest, ticket, seedOperatorID, at, at, at).Error; err != nil {
		return fmt.Errorf("insert ticket: %w", err)
	}
	ticketID, err := lastInsertID(tx)
	if err != nil {
		return err
	}
	if o.Status == string(model.OrderRejected) {
		if err := tx.Exec(`INSERT INTO rejection_reason (tenant_id, farm_id, ticket_id, order_id, advice_id, run_id, reason_code,
  reason_text, operator_id, operator_role, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sc.TenantID, sc.FarmID, ticketID, orderID, adviceID, runID, o.RejectCode, o.RejectText, seedOperatorID,
			seedOperatorRole, at).Error; err != nil {
			return fmt.Errorf("insert rejection reason: %w", err)
		}
		return nil
	}
	if err := tx.Exec(`INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status,
  actor_type, actor_id, reason_code, created_at) VALUES (?, ?, ?, ?, ?, ?, 'user', ?, ?, ?)`,
		sc.TenantID, sc.FarmID, orderID, runID, string(model.OrderApproved), o.Status, sc.UserID, o.CancelReason,
		at).Error; err != nil {
		return fmt.Errorf("insert cancel transition: %w", err)
	}
	return nil
}

// seedMeasured 写一条时效工具结果（time_sensitive=1）；Superseded 时 superseded_at 取同一时刻。
// call_key 按 run_id 与序号构造，在 trial 库内唯一。
func seedMeasured(tx *repository.DB, s SessionSeed, runID uint64, i int, m sample.SessionMeasured, at time.Time) error {
	result, err := json.Marshal(m.Result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	var superseded any
	if m.Superseded {
		superseded = at
	}
	sc := s.Scope
	if err := tx.Exec(`INSERT INTO tool_call (tenant_id, farm_id, run_id, step_id, call_key, attempt, tool_name, policy_level,
  args_digest, status, time_sensitive, result_summary, latency_ms, superseded_at, created_at)
  VALUES (?, ?, ?, 'enrich', ?, 1, ?, 'read', ?, 'OK', 1, ?, 0, ?, ?)`,
		sc.TenantID, sc.FarmID, runID, fmt.Sprintf("s%015d%016d", runID, i), m.Tool, seedDigest, string(result),
		superseded, at).Error; err != nil {
		return fmt.Errorf("insert tool call: %w", err)
	}
	return nil
}

// lastInsertID 读本连接上一条 INSERT 的自增 id（须在同一事务内调用）。
func lastInsertID(tx *repository.DB) (uint64, error) {
	var id uint64
	if err := tx.Raw(`SELECT LAST_INSERT_ID()`).Scan(&id).Error; err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	if id == 0 {
		return 0, fmt.Errorf("%w: last insert id is 0", errSessionSeed)
	}
	return id, nil
}
