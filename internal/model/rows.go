package model

import (
	"encoding/json"
	"time"
)

// 本文件是 L1 仓储读写用的表行结构（只列本层用到的列）；契约结构见 contracts.go。

// FarmProfile 是 farm_profile 行（按 uk_farm upsert）。
type FarmProfile struct {
	TenantID  uint64   `json:"tenant_id"`
	FarmID    uint64   `json:"farm_id"`
	Name      string   `json:"name"`
	Region    string   `json:"region"`
	Timezone  string   `json:"timezone"`
	MainCrops []string `json:"main_crops"`
}

// FieldProfile 是 field_profile 行（按 uk_ext upsert）；ID 即全系统 field_id。
type FieldProfile struct {
	ID           uint64          `json:"id"`
	TenantID     uint64          `json:"tenant_id"`
	FarmID       uint64          `json:"farm_id"`
	ExtFieldCode string          `json:"ext_field_code"`
	Crop         string          `json:"crop"`
	SowingDate   time.Time       `json:"sowing_date"`
	AreaMu       float64         `json:"area_mu"`
	Prefs        json.RawMessage `json:"prefs"`
}

// OrderState 是迁移前读取的作业单状态快照：乐观锁基于 RowVer。
type OrderState struct {
	RunID  uint64
	Status WorkOrderStatus
	RowVer int
}

// OrderTransition 是 work_order_transition 一行（只追加）。
type OrderTransition struct {
	Ref        OrderRef
	RunID      uint64
	From       WorkOrderStatus
	To         WorkOrderStatus
	Actor      Actor
	ReasonCode ReasonCode // 空串写 NULL
	CreatedAt  time.Time
}

// RunSummary 是 agent_run 的状态视图（GET /runs/{run_id}，L4）。
type RunSummary struct {
	RunID      uint64    `json:"run_id"`
	FieldID    uint64    `json:"field_id"`
	UserID     uint64    `json:"user_id"`
	Status     RunStatus `json:"status"`
	ReasonCode string    `json:"reason_code,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// AuditRecord 是 audit_log 一行（L7 由 run_event 投影写入）。
type AuditRecord struct {
	TenantID      uint64
	FarmID        uint64
	RunID         *uint64
	Actor         Actor
	Action        string
	Target        string
	PayloadDigest string
	Summary       json.RawMessage
	Result        string
	RawObjectID   *uint64
	CreatedAt     time.Time
}
