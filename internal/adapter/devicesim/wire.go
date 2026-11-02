// Package devicesim 是对接本地设备模拟平台（cmd/devicesim）的 DeviceAdapter（P1 §6.3 功能任务 4）：HTTP 客户端，
// 地址与超时来自 configs/devices.yaml，只用于本地 / 预发演练（配置里显式开启）。
// 平台协议（wire.go）刻意与标准回执不同名，由 receiptOf 转换，演示「各 Adapter 自己转协议」。
package devicesim

import (
	"encoding/json"
	"time"
)

// 平台任务状态（devicesim 协议）。
const (
	JobAccepted = "accepted"
	JobRunning  = "running"
	JobFinished = "finished"
	JobPartial  = "partial"
	JobFailed   = "failed"
	JobCanceled = "canceled"
)

// 平台失败阶段（devicesim 协议，仅 failed）。
const (
	StageBeforeStart = "before_start"
	StageInProgress  = "in_progress"
	StageUnknown     = "unknown"
)

// 平台错误码（ErrorBody.Code）。
const (
	CodeBadRequest  = "BAD_REQUEST"
	CodeNotFound    = "NOT_FOUND"
	CodeIdemClash   = "IDEM_CONFLICT" // 同一 idem_key 提交了不同作业单
	CodeUnavailable = "UNAVAILABLE"
	CodeRejected    = "REJECTED" // 脚本：平台拒收（设备未动作）
)

// SubmitRequest 是 POST /v1/jobs 的请求体（也是归档的指令原文）。
type SubmitRequest struct {
	IdemKey    string          `json:"idem_key"`
	TenantID   uint64          `json:"tenant_id"`
	FarmID     uint64          `json:"farm_id"`
	OrderID    uint64          `json:"order_id"`
	RunID      uint64          `json:"run_id"`
	FieldID    uint64          `json:"field_id"`
	DeviceType string          `json:"device_type"`
	WorkType   string          `json:"work_type"`
	Payload    json.RawMessage `json:"payload"`
	From       time.Time       `json:"window_from"`
	To         time.Time       `json:"window_to"`
}

// Job 是平台任务（受理 / 查询 / 撤销的统一返回）。
type Job struct {
	JobID      string    `json:"job_id"`
	ExternalNo string    `json:"external_no"` // = idem_key
	OrderID    uint64    `json:"order_id"`
	State      string    `json:"state"`
	FailStage  string    `json:"fail_stage,omitempty"`
	DoneMu     *float64  `json:"done_mu,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	ResultCode string    `json:"result_code"`
}

// ErrorBody 是平台非 2xx 的响应体。
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DeviceInfo 是 GET /v1/devices 的一项（mcp-device real 取在线 / 续航 / 载药 / 能力）。
type DeviceInfo struct {
	DeviceID     string       `json:"device_id"`
	TenantID     uint64       `json:"tenant_id"`
	FarmID       uint64       `json:"farm_id"`
	Kind         string       `json:"kind"`
	Model        string       `json:"model"`
	Capabilities []string     `json:"capabilities"`
	Fields       []string     `json:"fields"`
	Status       DeviceStatus `json:"status"`
}

// DeviceStatus 是设备实时状态。
type DeviceStatus struct {
	Online     bool      `json:"online"`
	State      string    `json:"state"`
	BatteryPct *float64  `json:"battery_pct,omitempty"`
	TankL      *float64  `json:"tank_l,omitempty"`
	FaultCode  string    `json:"fault_code,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// DeviceList 是 GET /v1/devices 的响应。
type DeviceList struct {
	Source    string       `json:"source"`
	UpdatedAt time.Time    `json:"updated_at"`
	Devices   []DeviceInfo `json:"devices"`
}

// Step 是脚本中的一次状态推进（每次查询推进一步）。
type Step struct {
	State     string   `json:"state"`
	FailStage string   `json:"fail_stage,omitempty"`
	DoneMu    *float64 `json:"done_mu,omitempty"`
}

// 脚本的受理结果（Script.Submit）。
const (
	SubmitAccept     = "accept"      // 受理（默认）
	SubmitReject     = "reject"      // 4xx 拒收
	SubmitError      = "error"       // 5xx，不建任务
	SubmitBadReceipt = "bad_receipt" // 受理但回执不合规（状态未登记）
)

// Script 是 PUT /sim/v1/scripts/{order_id} 的请求体：脚本化该作业单的回执序列。
// Steps 为空表示受理后一直无回执（NO_RECEIPT 演练）；DelayMS 是受理前的延迟毫秒数（超时演练：平台未建任务）；
// AcceptDelayMS 是「先受理建任务、再延迟响应」的毫秒数（超时演练：平台已建任务、客户端结果未知，需按 idem_key 查询）。
// 未登记脚本的作业单按 DefaultScript 推进。
type Script struct {
	Submit        string `json:"submit,omitempty"`
	DelayMS       int64  `json:"delay_ms,omitempty"`
	AcceptDelayMS int64  `json:"accept_delay_ms,omitempty"`
	Steps         []Step `json:"steps"`
}

// DefaultScript 是未登记脚本时的回执序列：受理 → 执行中 → 完成。
func DefaultScript() Script {
	return Script{Submit: SubmitAccept, Steps: []Step{{State: JobRunning}, {State: JobFinished}}}
}
