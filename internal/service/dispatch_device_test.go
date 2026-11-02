package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// scriptedTools 是 mcp-device 的桩：device.list 返回 list，device.status 按 device_id 取 status；
// fail 中的工具（device.list / device.status）原样返回该结果，statusFail 中的设备 device.status 原样返回该结果；
// status 里没有的设备返回 REJECTED/SCOPE_DENIED（gateway 归属校验拒绝）。
type scriptedTools struct {
	list       []string
	status     map[string]deviceStatusOut
	fail       map[string]agent.ToolResult
	statusFail map[string]agent.ToolResult
	err        error
	calls      []agent.ToolCall
}

var (
	resScopeDenied = agent.ToolResult{Status: model.ToolStatusRejected, ErrorCode: "SCOPE_DENIED"}
	resTimeout     = agent.ToolResult{Status: model.ToolStatusTimeout, ErrorCode: model.ToolStatusTimeout}
	resUpstream    = agent.ToolResult{Status: model.ToolStatusError, ErrorCode: toolCodeUpstream}
	resBadData     = agent.ToolResult{Status: model.ToolStatusOK, Data: json.RawMessage(`"not an object"`)}
)

func (s *scriptedTools) Call(_ context.Context, _ model.Scope, call agent.ToolCall) (agent.ToolResult, error) {
	s.calls = append(s.calls, call)
	if s.err != nil {
		return agent.ToolResult{}, s.err
	}
	if res, ok := s.fail[call.Tool]; ok {
		return res, nil
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
		if res, ok := s.statusFail[args.DeviceID]; ok {
			return res, nil
		}
		st, ok := s.status[args.DeviceID]
		if !ok {
			return resScopeDenied, nil
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

// ToolDevicePicker：只从本地块该类设备列表里选在线空闲的，已绑定优先；只有工具 OK 且确无在线空闲设备才是
// ErrDeviceUnavailable（离线）；工具超时 / 上游错误 / 归属校验拒绝 / 数据非法 → ErrDeviceToolFailed 且不是离线
// （审查修复 L6-7；应失败：把 TIMEOUT / SCOPE_DENIED 当离线，或拒绝时仍返回设备、选了列表外的已绑定设备）；
// 某台 status 失败但另一台可用 → 照常选中；无设备类别 → ErrDeviceNotBindable；传输错误原样返回。
func TestToolDevicePicker(t *testing.T) {
	order := model.WorkOrder{TenantID: 1, FarmID: 1, OrderID: 3, RunID: 5, FieldID: 8, DeviceType: "drone",
		Payload: json.RawMessage(`{"device_id":"uav-2"}`)}
	boom := errors.New("gateway down")
	offline := deviceStatusOut{Online: false, State: "offline"}
	for name, tc := range map[string]struct {
		tools      *scriptedTools
		deviceType string
		want       string
		wantErr    error
		transient  bool // 仅 wantErr == ErrDeviceToolFailed 时检查
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
		"status timeout other idle": {tools: &scriptedTools{list: []string{"uav-2", "uav-5"},
			status:     map[string]deviceStatusOut{"uav-5": idle("uav-5")},
			statusFail: map[string]agent.ToolResult{"uav-2": resTimeout}}, want: "uav-5"},
		"list scope denied": {tools: &scriptedTools{list: []string{"uav-2"}, status: map[string]deviceStatusOut{
			"uav-2": idle("uav-2")}, fail: map[string]agent.ToolResult{toolDeviceList: resScopeDenied}},
			wantErr: ErrDeviceToolFailed},
		"list timeout": {tools: &scriptedTools{list: []string{"uav-2"}, status: map[string]deviceStatusOut{
			"uav-2": idle("uav-2")}, fail: map[string]agent.ToolResult{toolDeviceList: resTimeout}},
			wantErr: ErrDeviceToolFailed, transient: true},
		"list upstream": {tools: &scriptedTools{fail: map[string]agent.ToolResult{toolDeviceList: resUpstream}},
			wantErr: ErrDeviceToolFailed, transient: true},
		"list bad data": {tools: &scriptedTools{fail: map[string]agent.ToolResult{toolDeviceList: resBadData}},
			wantErr: ErrDeviceToolFailed},
		"status timeout only device": {tools: &scriptedTools{list: []string{"uav-2"},
			statusFail: map[string]agent.ToolResult{"uav-2": resTimeout}}, wantErr: ErrDeviceToolFailed, transient: true},
		"status denied others offline": {tools: &scriptedTools{list: []string{"uav-2", "uav-5"},
			status: map[string]deviceStatusOut{"uav-5": offline}}, wantErr: ErrDeviceToolFailed},
		"status bad data": {tools: &scriptedTools{list: []string{"uav-2"},
			statusFail: map[string]agent.ToolResult{"uav-2": resBadData}}, wantErr: ErrDeviceToolFailed},
		"all offline": {tools: &scriptedTools{list: []string{"uav-2"},
			status: map[string]deviceStatusOut{"uav-2": {Online: false}}}, wantErr: ErrDeviceUnavailable},
		"offline and busy": {tools: &scriptedTools{list: []string{"uav-2", "uav-3"},
			status: map[string]deviceStatusOut{"uav-2": offline, "uav-3": {Online: true, State: "working"}}},
			wantErr: ErrDeviceUnavailable},
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
				if tc.wantErr != ErrDeviceUnavailable && errors.Is(err, ErrDeviceUnavailable) {
					t.Fatalf("err %v classified as offline", err)
				}
				var te *deviceToolError
				if errors.As(err, &te) != (tc.wantErr == ErrDeviceToolFailed) {
					t.Fatalf("err %v: deviceToolError = %t", err, te != nil)
				}
				if te != nil && te.transient() != tc.transient {
					t.Fatalf("err %v transient = %t, want %t", err, te.transient(), tc.transient)
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

// 下发器经 ToolDevicePicker 选设备（审查修复 L6-7）：工具 OK 且全部离线 → 推后离线（ErrOutboxDeviceOffline）、通知一次、
// 写 DEVICE_UNAVAILABLE 与 DEGRADED；工具超时 / 上游错误 / 归属校验拒绝 → 一般错误退避（不推后、不通知、不审计、
// 无 DEGRADED），非超时类另记 Warn（应失败：TIMEOUT / SCOPE_DENIED 被当成离线发通知）。
func TestDispatch_DeviceToolFailureNotOffline(t *testing.T) {
	const warnMsg = "dispatch: device tool rejected"
	for name, tc := range map[string]struct {
		tools   *scriptedTools
		offline bool
		warn    bool
	}{
		"all offline": {tools: &scriptedTools{list: []string{"uav-1"},
			status: map[string]deviceStatusOut{"uav-1": {Online: false, State: "offline"}}}, offline: true},
		"list timeout":  {tools: &scriptedTools{fail: map[string]agent.ToolResult{toolDeviceList: resTimeout}}},
		"list upstream": {tools: &scriptedTools{fail: map[string]agent.ToolResult{toolDeviceList: resUpstream}}},
		"list scope denied": {tools: &scriptedTools{fail: map[string]agent.ToolResult{toolDeviceList: resScopeDenied}},
			warn: true},
		"status timeout": {tools: &scriptedTools{list: []string{"uav-1"},
			statusFail: map[string]agent.ToolResult{"uav-1": resTimeout}}},
		"status scope denied": {tools: &scriptedTools{list: []string{"uav-1"}}, warn: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchFixture(t, model.OrderApproved, false)
			var logs bytes.Buffer
			deps := f.deps
			deps.Picker = ToolDevicePicker{Tools: tc.tools}
			deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			if err := f.svc.SetDispatch(deps); err != nil {
				t.Fatal(err)
			}
			err := f.svc.Dispatch(context.Background(), dispatchMsg(okDispatchPayload))
			if f.dev.dryRuns != 0 || f.store.order.Status != string(model.OrderApproved) {
				t.Fatalf("dryRuns=%d status=%s, want no dispatch", f.dev.dryRuns, f.store.order.Status)
			}
			if tc.offline {
				if !errors.Is(err, ErrOutboxDeviceOffline) || len(f.notify.rows) != 1 || len(f.audit.recs) != 1 ||
					len(f.degradedEvents(t)) != 1 {
					t.Fatalf("err=%v notify=%d audit=%d, want device offline deferral", err, len(f.notify.rows), len(f.audit.recs))
				}
				return
			}
			if err == nil || errors.Is(err, ErrOutboxDeviceOffline) || errors.Is(err, ErrOutboxDeferred) ||
				!errors.Is(err, ErrDeviceToolFailed) {
				t.Fatalf("err = %v, want plain ErrDeviceToolFailed (backoff)", err)
			}
			if f.notify.calls != 0 || len(f.audit.recs) != 0 || len(f.degradedEvents(t)) != 0 {
				t.Fatalf("notify=%d audit=%d DEGRADED=%d, want none", f.notify.calls, len(f.audit.recs), len(f.degradedEvents(t)))
			}
			if got := strings.Contains(logs.String(), warnMsg); got != tc.warn {
				t.Fatalf("warn logged = %t, want %t\n%s", got, tc.warn, logs.String())
			}
		})
	}
}
