package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agri-steward/internal/model"
)

// MockName 是 MockAdapter 的名称（写入 dispatch_result.adapter）。
const MockName = "mock"

var (
	// ErrRealDispatchDisabled 表示 P0 不允许真实下发（enable_robot_dispatch 固定关）。
	ErrRealDispatchDisabled = errors.New("adapter: real dispatch disabled in P0")
	// ErrUnsupportedWork 表示作业类型无对应的设备指令。
	ErrUnsupportedWork = errors.New("adapter: unsupported work type")
)

// Command 是一条将下发的设备指令（dry-run 只生成、不发送）。
type Command struct {
	Seq        int             `json:"seq"`
	Op         string          `json:"op"`
	DeviceType string          `json:"device_type"`
	FieldID    uint64          `json:"field_id"`
	Product    string          `json:"product,omitempty"`
	DosePerMu  json.RawMessage `json:"dose_per_mu,omitempty"`
	Total      json.RawMessage `json:"total,omitempty"`
	AreaMu     json.RawMessage `json:"area_mu,omitempty"`
	From       *time.Time      `json:"window_from,omitempty"`
	To         *time.Time      `json:"window_to,omitempty"`
}

// 设备指令操作码。
const (
	OpLoad       = "LOAD"
	OpSpray      = "SPRAY"
	OpSpread     = "SPREAD"
	OpValveOpen  = "VALVE_OPEN"
	OpValveClose = "VALVE_CLOSE"
	OpReturn     = "RETURN"
)

// orderPayload 是 work_order.payload 中指令需要的字段（其余字段忽略）。
type orderPayload struct {
	Product   string          `json:"product"`
	DosePerMu json.RawMessage `json:"dose_per_mu"`
	Total     json.RawMessage `json:"total"`
	AreaMu    json.RawMessage `json:"area_mu"`
}

// MockAdapter 是 P0 的设备 Adapter：DryRun 按 work_type 把 payload 转成指令数组（纯计算，无 IO），
// Dispatch 一律返回 ErrRealDispatchDisabled。零值可用。
type MockAdapter struct{}

var _ DeviceAdapter = MockAdapter{}

// Name 返回 MockName。
func (MockAdapter) Name() string { return MockName }

// DryRun 实现 DeviceAdapter。
func (MockAdapter) DryRun(ctx context.Context, order model.WorkOrder) (DryRunResult, error) {
	if err := ctx.Err(); err != nil {
		return DryRunResult{}, err
	}
	var p orderPayload
	if len(order.Payload) > 0 {
		if err := json.Unmarshal(order.Payload, &p); err != nil {
			return DryRunResult{}, fmt.Errorf("adapter: order %d payload: %w", order.OrderID, err)
		}
	}
	cmds, err := commands(order, p)
	if err != nil {
		return DryRunResult{}, err
	}
	for i := range cmds {
		cmds[i].Seq = i + 1
		cmds[i].DeviceType = order.DeviceType
		cmds[i].FieldID = order.FieldID
	}
	body, err := json.Marshal(cmds)
	if err != nil {
		return DryRunResult{}, fmt.Errorf("adapter: encode commands: %w", err)
	}
	return DryRunResult{Commands: body}, nil
}

// commands 按作业类型生成指令序列（Seq / 设备 / 地块由调用方统一填）。
func commands(order model.WorkOrder, p orderPayload) ([]Command, error) {
	from, to := order.TimeWindow.From, order.TimeWindow.To
	work := Command{Product: p.Product, DosePerMu: p.DosePerMu, Total: p.Total, AreaMu: p.AreaMu, From: &from, To: &to}
	switch order.WorkType {
	case model.WorkSpray:
		work.Op = OpSpray
		return []Command{{Op: OpLoad, Product: p.Product, Total: p.Total}, work, {Op: OpReturn}}, nil
	case model.WorkFertilize:
		work.Op = OpSpread
		return []Command{{Op: OpLoad, Product: p.Product, Total: p.Total}, work, {Op: OpReturn}}, nil
	case model.WorkIrrigate:
		work.Op = OpValveOpen
		return []Command{work, {Op: OpValveClose, To: &to}}, nil
	default:
		return nil, fmt.Errorf("%w: order %d work_type %q", ErrUnsupportedWork, order.OrderID, order.WorkType)
	}
}

// Dispatch 实现 DeviceAdapter：P0 不真实下发。
func (MockAdapter) Dispatch(context.Context, model.WorkOrder, string) (string, error) {
	return "", ErrRealDispatchDisabled
}
