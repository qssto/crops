package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// scriptedTools 是 mcp-device 的桩：device.list 返回 list，device.status 按 device_id 取 status；
// denied 中的工具返回 REJECTED/SCOPE_DENIED（gateway 归属校验拒绝）。
type scriptedTools struct {
	list   []string
	status map[string]deviceStatusOut
	denied map[string]bool
	err    error
	calls  []agent.ToolCall
}

func (s *scriptedTools) Call(_ context.Context, _ model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	s.calls = append(s.calls, call)
	if s.err != nil {
		return agent.ToolResult{}, s.err
	}
	if s.denied[call.Tool] {
		return agent.ToolResult{Status: model.ToolStatusRejected, ErrorCode: "SCOPE_DENIED"}, nil
	}
	var data any
	switch call.Tool {
	case toolDeviceList:
		var out deviceListOut
		for _, id := range s.list {
			out.Devices = append(out.Devices, struct {
				DeviceID string `json:"device_id"`
			}{id})
		}
		data = out
	case toolDeviceStatus:
		var args struct {
			DeviceID string `json:"device_id"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return agent.ToolResult{}, err
		}
		st, ok := s.status[args.DeviceID]
		if !ok {
			return agent.ToolResult{Status: model.ToolStatusRejected, ErrorCode: "SCOPE_DENIED"}, nil
		}
		data = st
	default:
		return agent.ToolResult{}, fmt.Errorf("unexpected tool %s", call.Tool)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Status: model.ToolStatusOK, Data: b}, nil
}

func idle(id string) deviceStatusOut {
	return deviceStatusOut{DeviceID: id, Online: true, State: deviceStateIdle}
}

// ToolDevicePicker：只从本地块该类设备列表里选在线空闲的，已绑定优先；归属校验拒绝 / 全部离线 → ErrDeviceUnavailable
// （应失败：拒绝时仍返回设备，或选了列表外的已绑定设备）；无设备类别 → ErrDeviceNotBindable；传输错误原样返回。
func TestToolDevicePicker(t *testing.T) {
	order := model.WorkOrder{TenantID: 1, FarmID: 1, OrderID: 3, RunID: 5, FieldID: 8, DeviceType: "drone",
		Payload: json.RawMessage(`{"device_id":"uav-2"}`)}
	boom := errors.New("gateway down")
	for name, tc := range map[string]struct {
		tools      *scriptedTools
		deviceType string
		want       string
		wantErr    error
	}{
		"bound device preferred": {tools: &scriptedTools{list: []string{"uav-1", "uav-2"},
			status: map[string]deviceStatusOut{"uav-1": idle("uav-1"), "uav-2": idle("uav-2")}}, want: "uav-2"},
		"offline and busy skipped": {tools: &scriptedTools{list: []string{"uav-2", "uav-3", "uav-4"},
			status: map[string]deviceStatusOut{"uav-2": {Online: false, State: deviceStateIdle},
				"uav-3": {Online: true, State: "working"}, "uav-4": idle("uav-4")}}, want: "uav-4"},
		"status denied skipped": {tools: &scriptedTools{list: []string{"uav-2", "uav-5"},
			status: map[string]deviceStatusOut{"uav-5": idle("uav-5")}}, want: "uav-5"},
		"bound device not listed": {tools: &scriptedTools{list: []string{"uav-1"},
			status: map[string]deviceStatusOut{"uav-1": idle("uav-1"), "uav-2": idle("uav-2")}}, want: "uav-1"},
		"list scope denied": {tools: &scriptedTools{list: []string{"uav-2"}, status: map[string]deviceStatusOut{
			"uav-2": idle("uav-2")}, denied: map[string]bool{toolDeviceList: true}}, wantErr: ErrDeviceUnavailable},
		"all offline": {tools: &scriptedTools{list: []string{"uav-2"},
			status: map[string]deviceStatusOut{"uav-2": {Online: false}}}, wantErr: ErrDeviceUnavailable},
		"empty list":     {tools: &scriptedTools{}, wantErr: ErrDeviceUnavailable},
		"manual":         {tools: &scriptedTools{}, deviceType: "manual", wantErr: ErrDeviceNotBindable},
		"transport fail": {tools: &scriptedTools{err: boom}, wantErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			o := order
			if tc.deviceType != "" {
				o.DeviceType = tc.deviceType
			}
			got, err := ToolDevicePicker{Tools: tc.tools}.PickDevice(context.Background(), o, 4)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || got != "" {
					t.Fatalf("got %q err %v, want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v, want %q", got, err, tc.want)
			}
			for _, c := range tc.tools.calls {
				if c.StepID != dispatchDeviceStep || c.Epoch != 4 || c.RunID != 5 {
					t.Errorf("call = %+v", c)
				}
			}
		})
	}
}
