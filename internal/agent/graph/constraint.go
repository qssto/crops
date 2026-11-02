package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/guardrail/constraint"
	"agri-steward/internal/model"
)

// 设备工具（P1 L3：只在下发前复检、无人机作业时按作业单 payload.device_id 只读调用；主链路不调用 device.*）。
const (
	toolDeviceStatus = "device.status"
	argDeviceID      = "device_id"
)

// constraintChecks 经约束注册表（P1 L3，graph.New 要求必填，与开票前 Reviewer 同一实例）判定作业单 o 的必检集合
// （phase：开票前 PRE_APPROVAL / 下发前 PRE_DISPATCH）。attempt 留 0，由落库方取 ticket_seq / recheck_seq。
func (a *Agent) constraintChecks(ctx context.Context, s *state, phase model.ConstraintPhase, o model.WorkOrder,
	productID string) []model.ConstraintCheck {
	return a.cfg.Constraints.Evaluate(a.constraintInput(ctx, s, phase, o, productID))
}

// constraintInput 由 run 状态构造校验输入（只读）：工具结果、知识快照、所选药剂、本地同地块 REAL 单。
func (a *Agent) constraintInput(ctx context.Context, s *state, phase model.ConstraintPhase, o model.WorkOrder,
	productID string) *constraint.Input {
	in := &constraint.Input{Phase: phase, WorkType: o.WorkType, DeviceType: o.DeviceType, DispatchMode: o.DispatchMode,
		Now: s.now, Loc: a.loc, Sources: make(map[string]constraint.Source, len(s.tools)), PHIOf: s.env.maxPHI,
		Window: o.TimeWindow, DeviceID: payloadDeviceID(o.Payload)}
	if s.env.snap != nil { // 避免 typed-nil 接口：未载入知识时阈值全部缺失（失败关闭）
		in.KB = s.env.snap
	}
	for name, out := range s.tools {
		in.Sources[name] = a.constraintSource(s, out)
	}
	if area, ok := s.facts.num(factAreaMu); ok {
		in.AreaMu = &area
	}
	if productID != "" && s.env.snap != nil {
		in.Product = s.env.constraintProduct(s, productID)
	}
	in.LocalOrders, in.LocalLoaded = a.localOrders(ctx, s, o.OrderID)
	return in
}

// constraintSource：采集时刻取数据 collected_at，缺省为 now − data_freshness_sec。
func (a *Agent) constraintSource(s *state, o *toolOut) constraint.Source {
	at := s.now.Add(-time.Duration(o.Result.DataFreshnessSec) * time.Second)
	if t, ok := parseTime(o.Data[keyCollectedAt], a.loc); ok {
		at = t
	}
	return constraint.Source{Tool: o.Tool, Status: o.Result.Status, ToolCallID: o.Result.ToolCallID, Source: o.Result.Source,
		CollectedAt: at, Data: o.Data}
}

// localOrders 读本地同地块近期 REAL 单（回看 = 最长安全间隔期与重复作业窗口）；未装配或读取失败返回 loaded=false
// （repeat_operation 判失败，失败关闭）。
func (a *Agent) localOrders(ctx context.Context, s *state, excludeOrderID uint64) ([]agent.LocalOrder, bool) {
	if a.cfg.LocalOrders == nil {
		return nil, false
	}
	since := s.now.AddDate(0, 0, -s.env.lookbackDays())
	orders, err := a.cfg.LocalOrders.RecentOrders(ctx, s.q.Scope, s.q.FieldID, excludeOrderID, since)
	if err != nil {
		a.log.WarnContext(ctx, "constraint: load local orders", "run_id", s.runID, "field_id", s.q.FieldID, "err", err)
		return nil, false
	}
	return orders, true
}

// constraintProduct 是所选药剂在本作物 × 防治对象 × 区域下的登记情况；登记查询失败一律按未登记（失败关闭）。
func (e *env) constraintProduct(s *state, productID string) *constraint.Product {
	p := &constraint.Product{ID: productID, CommonName: e.commonName(productID), Ref: e.calc.RegistryRef()}
	if g, err := e.calc.Registration(e.use(s, productID)); err == nil {
		p.Registered, p.PHIDays = true, g.PHIDays
	}
	return p
}

// maxPHI 是农事记录 / 本地单里的药剂名在本作物上各登记的最长安全间隔期：按 id / 通用名 / 别名 / 去含量剂型的归一名
// 查登记表（knowledge.Registry.Resolve，含 candidate:false 的仅 PHI 来源条目）。查不到或无本作物登记返回 known=false
// （phi_interval 判失败）；有本作物登记但 phi_days 全为空返回 known=true、days=nil（PHI 不适用，不计入最早收获日）。
func (e *env) maxPHI(name string) (days *int, known bool) {
	if e.snap == nil {
		return nil, false
	}
	for _, p := range e.snap.Registry().Resolve(name) {
		for _, g := range p.Registrations {
			if g.Crop != e.crop {
				continue
			}
			known = true
			if g.PHIDays != nil && (days == nil || *g.PHIDays > *days) {
				d := *g.PHIDays
				days = &d
			}
		}
	}
	return days, known
}

// lookbackDays 是本地完成单回看天数：本作物登记最长安全间隔期与 interval 条目最长重复作业窗口取大，至少 1 天。
func (e *env) lookbackDays() int {
	days := 1
	if e.snap == nil {
		return days
	}
	for _, p := range e.snap.Registry().Products {
		for _, g := range p.Registrations {
			if g.Crop == e.crop && g.PHIDays != nil {
				days = max(days, *g.PHIDays)
			}
		}
	}
	if en, ok := e.snap.Entry(constraint.KBInterval); ok {
		for _, th := range en.Thresholds {
			days = max(days, int(th.Value))
		}
	}
	return days + 1 // 含当日
}

// deviceSources（下发前复检、无人机）：只取作业单 payload.device_id 这台设备的 device.status 并入 s.tools。
// payload 无 device_id 时不调用（device_battery / device_payload 因 Input.DeviceID 为空判失败，失败关闭）；
// 不再按 device.list 自选设备——设备须在派单时绑定进作业单。
func (a *Agent) deviceSources(ctx context.Context, s *state, o model.WorkOrder) error {
	id := payloadDeviceID(o.Payload)
	if id == "" {
		return nil
	}
	args, err := json.Marshal(map[string]string{argDeviceID: id})
	if err != nil {
		return fmt.Errorf("encode device.status args: %w", err)
	}
	st, err := a.callTool(ctx, s, toolDeviceStatus, args)
	if err != nil {
		return err
	}
	if s.tools == nil {
		s.tools = map[string]*toolOut{}
	}
	s.tools[st.Tool] = st
	return nil
}

// payloadDeviceID 取作业单 payload.device_id；payload 为空、不可解析或字段非字符串均返回空串（调用方判失败）。
func payloadDeviceID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p struct {
		DeviceID any `json:"device_id"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	id, ok := p.DeviceID.(string)
	if !ok {
		return ""
	}
	return id
}
