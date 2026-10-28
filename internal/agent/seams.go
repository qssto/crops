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

// RunMeta 是建 agent_run 时写入的版本信息（graph / prompt / 模型档位 / 知识版本）。
type RunMeta struct {
	GraphVersion  string
	PromptVersion string
	ModelTier     string
	KnowledgeRef  string
}

// RunStateStore 读写 agent_run / run_step（RunState 契约的持久化）。
// 实现：repository（L4）。SaveStep 照抄 P0 §6.5 run_step 行别名 upsert。
type RunStateStore interface {
	CreateRun(ctx context.Context, s model.RunState, userID, fieldID uint64, meta RunMeta) (runID uint64, err error)
	LoadRun(ctx context.Context, scope model.Scope, runID uint64) (model.RunState, error)
	SaveStep(ctx context.Context, scope model.Scope, runID uint64, step model.RunStep) error
	// FinishRun 把 RUNNING 的 run 置为终态（COMPLETED / FAILED）；reasonCode 可空。
	FinishRun(ctx context.Context, scope model.Scope, runID uint64, status model.RunStatus, reasonCode string) error
	// Heartbeat 刷新 agent_run.updated_at（与 lock:run 续期同步）。
	Heartbeat(ctx context.Context, scope model.Scope, runID uint64) error
}

// AdviceStore 在同一事务写 advice_card + advice_citation + episodic_memory(ADVICE)，
// 均按业务唯一键幂等（uk_run / uk_advice_seq）：同一 run 重跑返回已有 advice_id。
// 实现：repository（L4）。
type AdviceStore interface {
	SaveAdvice(ctx context.Context, scope model.Scope, card model.AdviceCard) (adviceID uint64, err error)
}

// OrderDraftStore 为作业类建议建 work_order(DRAFT)，uk_advice 保证一卡一单（重跑返回已有 order_id）。
// 同事务写 work_order_transition（from 空 → DRAFT）。实现：repository（L4）。
type OrderDraftStore interface {
	CreateDraft(ctx context.Context, scope model.Scope, order model.WorkOrder) (orderID uint64, err error)
}

// Lease 是一次 run 锁持有（lock:run:{run_id}，token 锁）。
type Lease interface {
	Renew(ctx context.Context) error
	Release(ctx context.Context) error
}

// RunLocker 取 run 锁；已被持有返回错误。实现：app 包装 repository.Locker（L4）。
type RunLocker interface {
	AcquireRun(ctx context.Context, runID uint64) (Lease, error)
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

// ToolCall 是一次工具调用请求。CallSeq 是同一 step 内同工具同入参的第几次调用（参与 call_key），
// Epoch 取 agent_run.resume_epoch（只对时效工具参与 call_key）。
type ToolCall struct {
	RunID   uint64
	StepID  string
	Tool    string
	Args    json.RawMessage
	CallSeq int
	Epoch   int
}

// ToolCallStore 是 tool_call 读写（P0 §6.3 流程 2、4），各方法自管事务。
// 实现：repository.ToolCallRepo（L2）。
type ToolCallStore interface {
	// Save 落一行：attempt 取该 call_key 已有最大值 + 1；OK 且时效时同事务作废同 run、同工具的旧 epoch 结果。
	Save(ctx context.Context, row model.ToolCallRow) (toolCallID uint64, err error)
	// FindOK 取该 call_key 最新的 OK 行（去重回查），没有返回 false。
	FindOK(ctx context.Context, scope model.Scope, callKey string) (model.ToolCallHit, bool, error)
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
