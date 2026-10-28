package agent

import (
	"context"
	"encoding/json"
	"time"

	"agri-steward/internal/model"
)

// 本文件是 agent / harness 依赖的注入接缝（母文档 §4.6：harness 的存储能力以接口定义在 agent 内，
// 由 repository 实现、在 app 装配注入）。L1 只冻结签名；除 RawStore 外均无实现，注释标明实现层。
// 这些接口方法各自管理事务，不暴露 *gorm.DB（agent 不 import gorm / repository）。

// RawStore 写原文类对象并登记 raw_object（母文档 §4.6.2-8）。
// 实现：repository.RawStore（L1，MinIO + MySQL）。
type RawStore interface {
	Put(ctx context.Context, p model.RawPut) (model.RawObject, error)
}

// RunStateStore 读写 agent_run / run_step（RunState 契约的持久化）。
// 实现：repository（L4 runner 落地时）。
type RunStateStore interface {
	CreateRun(ctx context.Context, s model.RunState, userID, fieldID uint64) (runID uint64, err error)
	LoadRun(ctx context.Context, scope model.Scope, runID uint64) (model.RunState, error)
	SaveStep(ctx context.Context, scope model.Scope, runID uint64, step model.RunStep) error
}

// CheckpointStore 是 run_checkpoint 读写，承载 Eino compose.CheckPointStore（只在中断时写）。
// 实现：repository（L6），超限 payload 经 RawStore 转存 MinIO。
type CheckpointStore interface {
	Get(ctx context.Context, checkpointID string) ([]byte, bool, error)
	Set(ctx context.Context, checkpointID string, payload []byte) error
}

// EventSink 是 harness 发事件的出口：分配 seq 并追加 run_event（各自事务）。
// 实现：repository.EventRepo 的非事务包装（L7）；service 内的同事务写入直接用 EventRepo.AppendTx。
type EventSink interface {
	Emit(ctx context.Context, e model.RunEvent) (seq int64, err error)
}

// AuditStore 追加 audit_log（只追加表）。
// 实现：repository（L7，由 run_event 投影）。
type AuditStore interface {
	Append(ctx context.Context, r model.AuditRecord) error
}

// ToolCall 是一次工具调用请求。
type ToolCall struct {
	RunID  uint64
	StepID string
	Tool   string
	Args   json.RawMessage
	Epoch  int
}

// ToolResult 是工具调用结果；Status 取 tool_call.status（OK / TIMEOUT / EMPTY / ERROR / REJECTED）。
type ToolResult struct {
	ToolCallID       uint64
	Status           string
	ErrorCode        string
	Source           string
	DataFreshnessSec int
	Data             json.RawMessage
}

// ToolProvider 是工具调用接缝。
// 实现：agent/tools/mcp gateway（L2）；评测用 FixtureMCP（L2，evalkit）。
type ToolProvider interface {
	Call(ctx context.Context, scope model.Scope, call ToolCall) (ToolResult, error)
}

// KnowledgeEntry 是一条按 kb_id@version 精确读取的知识条目。
type KnowledgeEntry struct {
	KBID    string
	Version string
	Body    json.RawMessage
}

// KnowledgeSource 是知识读取接缝（红线组件只按 kb_id@version 精确读取，不走检索）。
// 实现：agent/knowledge 渐进式加载器（L3）。
type KnowledgeSource interface {
	Load(ctx context.Context, kbID, version string) (KnowledgeEntry, error)
}

// MemoryItem 是一条情景记忆（episodic_memory）。
type MemoryItem struct {
	Kind       string
	RefID      uint64
	Summary    string
	OccurredAt time.Time
}

// MemoryStore 是记忆读取接缝：farm / field profile + 近 N 条 episodic_memory。
// 实现：agent/harness/memory + repository（L6）。
type MemoryStore interface {
	Recent(ctx context.Context, scope model.Scope, fieldID uint64, limit int) ([]MemoryItem, error)
}

// UserChannel 是向用户推送事件与补问（WAIT_INPUT）的出口。
// 实现：controller SSE 经 service 注入（L8）。
type UserChannel interface {
	Push(ctx context.Context, scope model.Scope, runID uint64, e model.RunEvent) error
}

// Approver 是审批闸门接缝：为作业单开票并在中断后取决策。
// 实现：service/approval（L5 开票，L6 恢复）。
type Approver interface {
	Open(ctx context.Context, scope model.Scope, order model.WorkOrder) (model.ApprovalTicket, error)
	Decision(ctx context.Context, scope model.Scope, ticketID uint64) (model.ApprovalTicket, error)
}
