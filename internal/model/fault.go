package model

// P1 L6 故障分类与降级矩阵（§6.7）：故障码、降级动作与 DEGRADED 事件 payload。
// 故障码 → 行为 → 文案 key → 告警项声明在 policy/fault_matrix.yaml（harness/policy.LoadFaultMatrix 启动期校验）。

// FaultCode 是一类故障的稳定错误码（fault_matrix.yaml 的键）。
type FaultCode string

const (
	FaultLLMUnavailable FaultCode = "LLM_UNAVAILABLE" // 模型重试后仍不可用（超时 / 5xx / 连接错误）
	FaultMCPTimeout     FaultCode = "MCP_TIMEOUT"     // MCP 工具 TIMEOUT 或 UPSTREAM
	FaultKBMiss         FaultCode = "KB_MISS"         // 知识库未覆盖
	FaultDeviceOffline  FaultCode = "DEVICE_OFFLINE"  // 下发器 ③ 选不到本农场在线设备
)

// FaultCodes 是矩阵必须列齐的全部故障码（缺一即启动失败）。
func FaultCodes() []FaultCode {
	return []FaultCode{FaultLLMUnavailable, FaultMCPTimeout, FaultKBMiss, FaultDeviceOffline}
}

// DegradeAction 是故障发生后实际采取的行为（fault_matrix.yaml 的 behavior / DEGRADED 事件的 action）。
type DegradeAction string

const (
	DegradeRuleFallback  DegradeAction = "RULE_FALLBACK"   // LLM 不可用：规则层保守建议，degraded=true
	DegradeCacheFallback DegradeAction = "CACHE_FALLBACK"  // MCP 超时：用 max_staleness 内的缓存，source='cache'
	DegradeRefuse        DegradeAction = "REFUSE"          // 无法降级：拒答（LLM 不可用不满足规则层条件 / 缓存超限或缺失 / 知识缺失）
	DegradeHoldNotify    DegradeAction = "HOLD_AND_NOTIFY" // 设备离线：不下发、工单状态不变、通知一次，消息推后 1 分钟
)

// AuditDegraded 是 DEGRADED 事件投影到 audit_log 的 action（result=OK：降级是预期行为，失败由业务结局体现）。
const AuditDegraded AuditAction = "DEGRADED"

// DegradedPayload 是 run_event(DEGRADED) 的 payload（§6.7 SQL：fault / action / staleness_sec），
// 另带文案 key 与定位字段；审计 target 取 fault:{fault}。
type DegradedPayload struct {
	Fault        FaultCode     `json:"fault"`
	Action       DegradeAction `json:"action"`
	StalenessSec *int          `json:"staleness_sec,omitempty"` // 仅 CACHE_FALLBACK：now - collected_at
	TextKey      string        `json:"text_key,omitempty"`      // 用户文案 key（configs/texts/zh-CN.yaml）
	Tool         string        `json:"tool,omitempty"`          // MCP_TIMEOUT：出错的工具
	OrderID      uint64        `json:"order_id,omitempty"`      // DEVICE_OFFLINE：作业单
	TicketID     uint64        `json:"ticket_id,omitempty"`     // DEVICE_OFFLINE：审批票据
}

// ToolSourceCache 是 MCP 网关在 TIMEOUT / UPSTREAM 后回落到 max_staleness 内缓存时写入 tool_call.source 与
// ToolResult.Source 的取值（上游来源名均为 mock-* / 真实数据源名，不会取 cache）。
const ToolSourceCache = "cache"
