package service

import (
	"strconv"
	"strings"
	"time"

	"agri-steward/internal/model"
)

// 指标名（P1 L7-B；规则文件 deploy/alerts/p1.yaml 只引用这里登记的名字，测试校验）。
const (
	MetricAlerts           = "as_alert_total"
	MetricDispatch         = "as_dispatch_total"
	MetricOrderTransitions = "as_order_transition_total"
	MetricApprovalBacklog  = "as_approval_backlog"
	MetricApprovalWait     = "as_approval_wait_seconds"
	MetricFlagChanges      = "as_flag_change_total"
	MetricRunResumes       = "as_run_resume_total"
	MetricLLMTokens        = "as_llm_tokens_total"
	MetricMCPCalls         = "as_mcp_calls_total"
	MetricDegraded         = "as_degraded_total"
)

// labelOther 是不在枚举内的取值（防止 label 基数失控）。
const labelOther = "other"

// approvalWaitBuckets 是审批等待直方图上界（秒）：1 分钟到 24 小时。
var approvalWaitBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800, 86400}

// 有限枚举 label 的取值表。
var (
	mcpServers    = map[string]bool{"weather": true, "crop": true, "field": true, "device": true}
	toolStatuses  = setOf(model.ToolStatusOK, model.ToolStatusTimeout, model.ToolStatusEmpty, model.ToolStatusError, model.ToolStatusRejected)
	alertLevels   = setOf(string(model.AlertP0), string(model.AlertP1))
	faultCodes    = setOf(string(model.FaultLLMUnavailable), string(model.FaultMCPTimeout), string(model.FaultKBMiss), string(model.FaultDeviceOffline))
	flagKeys      = setOf(string(model.FlagEnableAdvice), string(model.FlagEnableRobotDispatch))
	dispatchModes = setOf(string(model.DispatchDryRun), string(model.DispatchReal))
	reasonCodes   = setOf(string(model.ReasonApprovalRejected), string(model.ReasonApprovalExpired),
		string(model.ReasonConditionChanged), string(model.ReasonRetryExhausted), string(model.ReasonRecheckFailed),
		string(model.ReasonRetryBlocked), string(model.ReasonDispatchRejected), string(model.ReasonDeviceUnavailable),
		string(model.ReasonReceipt), string(model.ReasonReconciled), string(model.ReasonCanceledByUser))
	ticketDecisions = setOf(string(model.TicketApproved), string(model.TicketRejected))
	denyCodes       = setOf(DenyScope, DenyRole, DenyResource)
	knownAlerts     = alertNameSet()
)

func alertNameSet() map[string]bool {
	m := make(map[string]bool, len(model.AlertNames))
	for _, n := range model.AlertNames {
		m[string(n)] = true
	}
	return m
}

func setOf(vs ...string) map[string]bool {
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[v] = true
	}
	return m
}

// enum 返回 v（在表内）或 other。
func enum(set map[string]bool, v string) string {
	if set[v] {
		return v
	}
	return labelOther
}

func farmLabel(farmID uint64) string { return strconv.FormatUint(farmID, 10) }

// MetricsObserver 是 Observer 的注册表实现。
type MetricsObserver struct {
	reg         *MetricRegistry
	alerts      *metricFamily
	dispatch    *metricFamily
	transitions *metricFamily
	backlog     *metricFamily
	wait        *metricFamily
	denied      *metricFamily
	flags       *metricFamily
	resumes     *metricFamily
	tokens      *metricFamily
	mcp         *metricFamily
	degraded    *metricFamily
}

var _ Observer = (*MetricsObserver)(nil)

// NewMetricsObserver 在新注册表上登记全部业务指标，并预置无 farm_id 的有限枚举序列与 farm_id=0 的序列（Preseed）。
func NewMetricsObserver() *MetricsObserver {
	r := NewMetricRegistry()
	o := &MetricsObserver{reg: r,
		alerts:      r.register(MetricCounter, MetricAlerts, "Alerts fired via AlertSink.", nil, "alert", "level", "farm_id"),
		dispatch:    r.register(MetricCounter, MetricDispatch, "Device dispatch outcomes.", nil, "farm_id", "mode", "result"),
		transitions: r.register(MetricCounter, MetricOrderTransitions, "Committed work order transitions.", nil, "farm_id", "to", "reason"),
		backlog:     r.register(MetricGauge, MetricApprovalBacklog, "OPEN ORDER approval tickets (refreshed per scan interval).", nil, "farm_id"),
		wait: r.register(MetricHistogram, MetricApprovalWait, "Seconds from ticket creation to decision.", approvalWaitBuckets,
			"farm_id", "decision"),
		denied:   r.register(MetricCounter, DeniedMetric, "Access denials (CROSS_FARM_DENIED audit).", nil, "code"),
		flags:    r.register(MetricCounter, MetricFlagChanges, "Feature flag changes.", nil, "flag"),
		resumes:  r.register(MetricCounter, MetricRunResumes, "Run resume attempts after approval.", nil, "result"),
		tokens:   r.register(MetricCounter, MetricLLMTokens, "LLM tokens by kind (LLM_CALL_END).", nil, "farm_id", "kind"),
		mcp:      r.register(MetricCounter, MetricMCPCalls, "MCP tool calls by server and status.", nil, "server", "status"),
		degraded: r.register(MetricCounter, MetricDegraded, "DEGRADED events by fault code.", nil, "farm_id", "fault"),
	}
	for c := range denyCodes {
		o.denied.touch(c)
	}
	for k := range flagKeys {
		o.flags.touch(k)
	}
	for _, res := range []string{"ok", "error"} {
		o.resumes.touch(res)
	}
	for srv := range mcpServers {
		for st := range toolStatuses {
			o.mcp.touch(srv, st)
		}
	}
	o.Preseed(nil)
	return o
}

// dispatchOutcomes 是 as_dispatch_total 的 result 取值。
var dispatchOutcomes = []DispatchOutcome{DispatchOK, DispatchRejected, DispatchManual, DispatchError, DispatchBlocked}

// preseedTransitions 是告警规则引用的迁移 (to, reason)：审批 / 下发截止过期（APPROVAL_EXPIRED）与下发前复检退回（RECHECK_FAILED）。
var preseedTransitions = []struct {
	to     model.WorkOrderStatus
	reason model.ReasonCode
}{
	{model.OrderExpired, model.ReasonApprovalExpired},
	{model.OrderPendingApproval, model.ReasonRecheckFailed},
}

// Preseed 为 farmIDs（另加 0：平台链 / 无农场归属）预置 0 值计数器序列，已存在的不动（幂等）。
// 审查修复（P0）：计数器序列按需创建时首个样本即为 1，increase() 只看窗口内两点之差，第一次告警会漏；
// 预置后首次计数是 0 → 1，increase 可见。覆盖告警项 × 级别、降级故障码、下发 mode × 结果、token kind 与 preseedTransitions；
// 规则文件里「任一次即告警」的规则另有 unless/offset 新序列分支兜底（未登记的新农场在下一轮预置前也能触发）。
func (o *MetricsObserver) Preseed(farmIDs []uint64) {
	for _, id := range append([]uint64{0}, farmIDs...) {
		farm := farmLabel(id)
		for _, n := range model.AlertNames {
			for lv := range alertLevels {
				o.alerts.touch(string(n), lv, farm)
			}
		}
		for fc := range faultCodes {
			o.degraded.touch(farm, fc)
		}
		for mode := range dispatchModes {
			for _, res := range dispatchOutcomes {
				o.dispatch.touch(farm, mode, string(res))
			}
		}
		o.tokens.touch(farm, "prompt")
		o.tokens.touch(farm, "completion")
		for _, tr := range preseedTransitions {
			o.transitions.touch(farm, string(tr.to), string(tr.reason))
		}
	}
}

// Registry 返回底层注册表（/metrics 处理器与测试用）。
func (o *MetricsObserver) Registry() *MetricRegistry { return o.reg }

// IncDenied 实现 DenialCounter。
func (o *MetricsObserver) IncDenied(code string) {
	o.denied.add(1, enum(denyCodes, code))
}

// AlertFired 实现 Observer。
func (o *MetricsObserver) AlertFired(a model.Alert) {
	o.alerts.add(1, enum(knownAlerts, string(a.Name)), enum(alertLevels, string(a.Level)), farmLabel(a.Scope.FarmID))
}

// EventCommitted 实现 Observer。
func (o *MetricsObserver) EventCommitted(e model.RunEvent) {
	switch e.Type {
	case model.EventOrderTransition, model.EventDegraded, model.EventLLMCallEnd:
	default:
		return
	}
	f, ok := decodeEventFacts(e.Payload)
	if !ok {
		return
	}
	farm := farmLabel(e.FarmID)
	switch e.Type {
	case model.EventOrderTransition:
		to := labelOther
		if f.To.Valid() {
			to = string(f.To)
		}
		reason := "none"
		if f.ReasonCode != "" {
			reason = enum(reasonCodes, string(f.ReasonCode))
		}
		o.transitions.add(1, farm, to, reason)
	case model.EventDegraded:
		o.degraded.add(1, farm, enum(faultCodes, string(f.Fault)))
	case model.EventLLMCallEnd:
		if f.PromptTokens > 0 {
			o.tokens.add(float64(f.PromptTokens), farm, "prompt")
		}
		if f.CompletionTokens > 0 {
			o.tokens.add(float64(f.CompletionTokens), farm, "completion")
		}
	}
}

// Dispatched 实现 Observer。
func (o *MetricsObserver) Dispatched(farmID uint64, mode model.DispatchMode, result DispatchOutcome) {
	o.dispatch.add(1, farmLabel(farmID), enum(dispatchModes, string(mode)), string(result))
}

// ApprovalDecided 实现 Observer。
func (o *MetricsObserver) ApprovalDecided(farmID uint64, decision model.TicketStatus, wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	o.wait.observe(wait.Seconds(), farmLabel(farmID), enum(ticketDecisions, string(decision)))
}

// ApprovalBacklog 实现 Observer。
func (o *MetricsObserver) ApprovalBacklog(open map[uint64]int) {
	points := make(map[string]float64, len(open))
	for farm, n := range open {
		points[farmLabel(farm)] = float64(n)
	}
	o.backlog.replace(points, func(k string) []string { return []string{k} })
}

// FlagChanged 实现 Observer。
func (o *MetricsObserver) FlagChanged(flag model.FlagKey) {
	o.flags.add(1, enum(flagKeys, string(flag)))
}

// Resumed 实现 Observer。
func (o *MetricsObserver) Resumed(ok bool) {
	result := "error"
	if ok {
		result = "ok"
	}
	o.resumes.add(1, result)
}

// ToolCalled 实现 Observer。
func (o *MetricsObserver) ToolCalled(tool, status string) {
	server, _, _ := strings.Cut(tool, ".")
	o.mcp.add(1, enum(mcpServers, server), enum(toolStatuses, status))
}
