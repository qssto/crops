package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// audit_log 列宽与 summary 上限（P0 §6.8）。
const (
	auditTargetMax  = 64
	auditSummaryMax = 8 << 10
)

// ErrBadAuditPayload 表示要投影的事件 payload 不是合法 JSON 对象。
var ErrBadAuditPayload = errors.New("repository: bad audit payload")

const insertAuditSQL = `INSERT INTO audit_log (created_at, tenant_id, farm_id, run_id, actor_type, actor_id, action, target, payload_digest, summary, result, raw_object_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

func auditArgs(r model.AuditRecord) []any {
	var summary any
	if len(r.Summary) > 0 {
		summary = string(r.Summary)
	}
	return []any{r.CreatedAt, r.TenantID, r.FarmID, r.RunID, string(r.Actor.Type), r.Actor.ID, r.Action,
		nullIfEmpty(r.Target), r.PayloadDigest, summary, r.Result, r.RawObjectID}
}

// AuditRepo 写 audit_log：AppendTx 在调用方事务内写，Append 独立短写（拒绝类审计不随业务回滚）。
type AuditRepo struct{ DB *gorm.DB }

var _ agent.AuditStore = AuditRepo{}

// AppendTx 在调用方事务内追加一行审计。
func (AuditRepo) AppendTx(ctx context.Context, tx *gorm.DB, r model.AuditRecord) error {
	if _, err := execInTx(ctx, tx, insertAuditSQL, auditArgs(r)...); err != nil {
		return fmt.Errorf("repository: insert audit_log %s %s: %w", r.Action, r.Result, err)
	}
	return nil
}

// Append 独立短写一行审计。
func (a AuditRepo) Append(ctx context.Context, r model.AuditRecord) error {
	if a.DB == nil {
		return errors.New("repository: audit repo has no DB")
	}
	if err := a.DB.WithContext(ctx).Exec(insertAuditSQL, auditArgs(r)...).Error; err != nil {
		return fmt.Errorf("repository: insert audit_log %s %s: %w", r.Action, r.Result, err)
	}
	return nil
}

// NewAuditRecord 按 payload 生成审计行的摘要列：payload_digest = sha256(payload)，summary = payload（≤8KB，超出记长度）。
// payload 由事件发出方脱敏；这里不再改写内容。
func NewAuditRecord(scope model.Scope, runID *uint64, actor model.Actor, action model.AuditAction, target string,
	payload json.RawMessage, result string, at time.Time) model.AuditRecord {
	return model.AuditRecord{TenantID: scope.TenantID, FarmID: scope.FarmID, RunID: runID, Actor: actor,
		Action: string(action), Target: clipTarget(target), PayloadDigest: Digest(payload),
		Summary: auditSummary(payload), Result: result, CreatedAt: at}
}

func auditSummary(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > auditSummaryMax {
		return json.RawMessage(`{"truncated":true,"bytes":` + strconv.Itoa(len(payload)) + `}`)
	}
	return payload
}

// clipTarget 按字符截到 target 列宽（varchar(64)）。
func clipTarget(s string) string {
	r := []rune(s)
	if len(r) <= auditTargetMax {
		return s
	}
	return string(r[:auditTargetMax])
}

// auditKeys 是 target / result 取值所需的 payload 字段（各事件只用其中一个）。
type auditKeys struct {
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	OrderID  uint64 `json:"order_id"`
	TicketID uint64 `json:"ticket_id"`
	FieldID  uint64 `json:"field_id"`
	KBRef    string `json:"kb_ref"`
	Task     string `json:"task"`
	Check    string `json:"check"`
	AdviceID uint64 `json:"advice_id"`
}

func ref(kind string, id uint64) string { return kind + ":" + strconv.FormatUint(id, 10) }

// auditTargets：event_type → target（如 tool:weather.forecast、order:12、ticket:7）。
var auditTargets = map[model.EventType]func(auditKeys) string{
	model.EventRunStarted:       func(k auditKeys) string { return ref("field", k.FieldID) },
	model.EventToolCallEnd:      func(k auditKeys) string { return "tool:" + k.Tool },
	model.EventKBLoaded:         func(k auditKeys) string { return "kb:" + k.KBRef },
	model.EventLLMCallEnd:       func(k auditKeys) string { return "llm:" + k.Task },
	model.EventValidationFailed: func(k auditKeys) string { return "check:" + k.Check },
	model.EventAdviceReady:      func(k auditKeys) string { return ref("advice", k.AdviceID) },
	model.EventApprovalOpen:     func(k auditKeys) string { return ref("ticket", k.TicketID) },
	model.EventApprovalPending:  func(k auditKeys) string { return ref("ticket", k.TicketID) },
	model.EventApprovalDecided:  func(k auditKeys) string { return ref("ticket", k.TicketID) },
	model.EventOrderTransition:  func(k auditKeys) string { return ref("order", k.OrderID) },
	model.EventDispatched:       func(k auditKeys) string { return ref("order", k.OrderID) },
}

// auditResult：VALIDATION_FAILED → FAILED；TOOL_CALL_END 按 status（REJECTED → DENIED）；LLM_CALL_END 的 ERROR → FAILED；其余 OK。
func auditResult(t model.EventType, k auditKeys) string {
	switch t {
	case model.EventValidationFailed:
		return model.AuditResultFailed
	case model.EventToolCallEnd:
		switch k.Status {
		case model.ToolStatusOK:
			return model.AuditResultOK
		case model.ToolStatusRejected:
			return model.AuditResultDenied
		}
		return model.AuditResultFailed
	case model.EventLLMCallEnd:
		if k.Status == model.LLMStatusError {
			return model.AuditResultFailed
		}
	}
	return model.AuditResultOK
}

// ProjectAudit 把 run_event 投影为 audit_log 行；不在投影表内的事件返回 false。
func ProjectAudit(e model.RunEvent) (model.AuditRecord, bool, error) {
	action, ok := e.Type.AuditAction()
	if !ok {
		return model.AuditRecord{}, false, nil
	}
	var k auditKeys
	if len(e.Payload) == 0 {
		return model.AuditRecord{}, false, fmt.Errorf("%w: %s run %d: empty payload", ErrBadAuditPayload, e.Type, e.RunID)
	}
	if err := json.Unmarshal(e.Payload, &k); err != nil {
		return model.AuditRecord{}, false, fmt.Errorf("%w: %s run %d: %v", ErrBadAuditPayload, e.Type, e.RunID, err)
	}
	target := ""
	if fn := auditTargets[e.Type]; fn != nil {
		target = fn(k)
	}
	runID := e.RunID
	rec := NewAuditRecord(model.Scope{TenantID: e.TenantID, FarmID: e.FarmID}, &runID,
		model.Actor{Type: e.Actor, ID: e.ActorID}, action, target, e.Payload, auditResult(e.Type, k), e.CreatedAt)
	rec.RawObjectID = e.RawObjectID
	return rec, true, nil
}
