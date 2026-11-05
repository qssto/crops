package model

import "time"

// P2 L8 线上回流（§6.9 技术实现 3）：agent_run.backflow_status / backflow_draft_ref。
// 合成码与真实 farm_id / field_id 的映射只留在业务库 agent_run.backflow_draft_ref，草稿与评测库只见合成码。
const (
	BackflowSelected  = "SELECTED"   // 本周选中，待取原文脱敏
	BackflowDrafted   = "DRAFTED"    // 草稿已写出（pii_clean 通过）
	BackflowPIIFailed = "PII_FAILED" // 脱敏后 pii_clean 仍命中，草稿不写出
	BackflowMerged    = "MERGED"     // 专家补 expect 后并入回归集（待专家，本期只留状态）
	BackflowDropped   = "DROPPED"    // 专家判定不入库（待专家，本期只留状态）
	// BackflowFailed 是 L8 评审修复新增的终态（迁移注释的取值集合未改，dev-log 记录）：取原文 / 出草稿连续失败
	// BackflowMaxAttempts 次后置此状态，不再占 SelectedRuns 名额、不再重读原文。
	BackflowFailed = "FAILED"
)

// BackflowRetry 只出现在 BACKFLOW 审计事件里（不是 agent_run 状态）：一次失败的出草稿尝试。
const BackflowRetry = "RETRY"

// BackflowMaxAttempts 是单个 run 出草稿的最多尝试次数（含首次）。
const BackflowMaxAttempts = 3

// BackflowTool 是 run 内一次有原文的工具调用（tool_call 未被取代、raw_object_id 非空）。
type BackflowTool struct {
	Name  string
	RawID uint64
}

// BackflowMaterial 是生成回流草稿的业务库材料（真实 ID 只在内存里，草稿只写合成码）。
type BackflowMaterial struct {
	RunID, FieldID, UserID uint64
	Question               string    // audit_log ASK 的 summary.question（入库前已 pii_mask）
	AskedAt                time.Time // audit_log ASK 的时刻（草稿 fixtures.clock）
	KnowledgeRef           string    // audit_log KB_LOAD 的 kb_ref；无 KB_LOAD 时取 agent_run.knowledge_ref
	Crop                   string    // field_profile.crop
	Tools                  []BackflowTool
}
