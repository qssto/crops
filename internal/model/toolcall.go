package model

import (
	"encoding/json"
	"time"
)

// tool_call.status 取值（P0 §6.3）。
const (
	ToolStatusOK       = "OK"
	ToolStatusTimeout  = "TIMEOUT"
	ToolStatusEmpty    = "EMPTY"
	ToolStatusError    = "ERROR"
	ToolStatusRejected = "REJECTED"
)

// ToolCallRow 是 tool_call 一行（attempt 由 ToolCallStore.Save 在事务内取已有最大值 + 1，不由调用方给出）。
type ToolCallRow struct {
	TenantID         uint64
	FarmID           uint64
	RunID            uint64
	StepID           string
	CallKey          string // char(32)
	Epoch            int    // 非时效工具恒 0
	ToolName         string
	PolicyLevel      string // read / write / actuate
	ArgsDigest       string // char(64)
	Status           string // ToolStatus*
	ErrorCode        string // 空串写 NULL
	Source           string // 空串写 NULL
	DataFreshnessSec *int
	TimeSensitive    bool
	ResultSummary    json.RawMessage // 已脱敏；nil 写 NULL
	RawObjectID      *uint64
	LatencyMS        int
	CreatedAt        time.Time
}

// ToolCallHit 是去重回查命中的成功行（P0 §6.3 去重回查 SQL 的三列）。
type ToolCallHit struct {
	ID            uint64
	ResultSummary json.RawMessage
	RawObjectID   *uint64
}
