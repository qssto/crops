package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
)

// 设备选定（下发器第 ③ 步，P1 §6.5 技术实现第 3 条；L3 遗留义务①）。
var (
	// ErrDeviceUnavailable 表示本农场取不到可用设备（列表为空、全部离线 / 忙、归属校验失败、工具报错），
	// 下发器不调 Adapter、单状态不变、写 DEVICE_UNAVAILABLE 审计后推后重试。
	ErrDeviceUnavailable = errors.New("service: no available device")
	// ErrDeviceNotBindable 表示该 device_type 没有对应的设备类别（如 manual 人工作业），无设备可绑定。
	// DRY_RUN 单跳过绑定照常 dry-run；REAL 单按设备不可用处理（失败关闭）。
	ErrDeviceNotBindable = errors.New("service: device type has no bindable device")
)

// DevicePicker 按作业单 device_type 从本农场设备中选定一台在线空闲设备，返回 device_id。
// 选不到返回 ErrDeviceUnavailable，类型无设备类别返回 ErrDeviceNotBindable，其余错误为基础设施故障（退避重试）。
type DevicePicker interface {
	PickDevice(ctx context.Context, o model.WorkOrder, epoch int) (string, error)
}

// 设备工具名与参数（mcp-device，P0 §6.3）。
const (
	toolDeviceList   = "device.list"
	toolDeviceStatus = "device.status"
	// dispatchDeviceStep 是选设备调用的 step_id：与下发前复检同一 step（mcp gateway 对 pre_dispatch 一律不读缓存）。
	dispatchDeviceStep = "pre_dispatch"
	deviceStateIdle    = "idle"
)

// deviceKinds 是 work_order.device_type → mcp-device 设备类别（device.list 的 kind）。
// 假设（无设备主数据对照表，见 dev-log P1-L4-A）：uav / drone 是无人机，knapsack / vehicle 是地面喷雾，
// valve / fertigation 是水泵阀门；manual 等不在表内的类型无设备可绑定。
var deviceKinds = map[string]string{
	"uav": "drone", "drone": "drone",
	"knapsack": "sprayer", "vehicle": "sprayer",
	"valve": "pump", "fertigation": "pump",
}

// ToolDevicePicker 经 mcp-device（gateway 的租户 / 地块 / 设备归属校验）选设备：device.list{field_id, kind} 列本地块该类设备，
// 已绑定的设备优先，逐台 device.status 取第一台 online 且 idle 的。归属校验拒绝（SCOPE_DENIED 等非 OK）一律按不可用，失败关闭。
type ToolDevicePicker struct{ Tools agent.ToolProvider }

type deviceListOut struct {
	Devices []struct {
		DeviceID string `json:"device_id"`
	} `json:"devices"`
}

type deviceStatusOut struct {
	DeviceID string `json:"device_id"`
	Online   bool   `json:"online"`
	State    string `json:"state"`
}

// PickDevice 实现 DevicePicker。
func (p ToolDevicePicker) PickDevice(ctx context.Context, o model.WorkOrder, epoch int) (string, error) {
	if p.Tools == nil {
		return "", fmt.Errorf("service: device picker: %w", ErrDispatchNotConfigured)
	}
	kind, ok := deviceKinds[o.DeviceType]
	if !ok {
		return "", fmt.Errorf("%w: device_type %q", ErrDeviceNotBindable, o.DeviceType)
	}
	scope := model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}
	var list deviceListOut
	if err := p.call(ctx, scope, o, epoch, toolDeviceList, 0,
		map[string]any{"field_id": o.FieldID, "kind": kind}, &list); err != nil {
		return "", err
	}
	// 候选只来自本地块该类设备列表；已绑定的设备在列表里时排第一（不在列表里即不再采用）
	bound := payloadDeviceID(o.Payload)
	ids := make([]string, 0, len(list.Devices))
	for _, d := range list.Devices {
		switch {
		case d.DeviceID == "":
		case d.DeviceID == bound:
			ids = append([]string{bound}, ids...)
		default:
			ids = append(ids, d.DeviceID)
		}
	}
	for i, id := range ids {
		var st deviceStatusOut
		err := p.call(ctx, scope, o, epoch, toolDeviceStatus, i+1, map[string]any{"device_id": id}, &st)
		if errors.Is(err, ErrDeviceUnavailable) {
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Online && st.State == deviceStateIdle {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: order %d field %d kind %s (%d listed)", ErrDeviceUnavailable, o.OrderID, o.FieldID, kind,
		len(list.Devices))
}

// call 调一次设备工具并解码 data；工具返回非 OK（含归属校验拒绝）→ ErrDeviceUnavailable，传输层错误原样返回。
func (p ToolDevicePicker) call(ctx context.Context, scope model.Scope, o model.WorkOrder, epoch int, tool string, seq int,
	args map[string]any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("service: encode %s args: %w", tool, err)
	}
	res, err := p.Tools.Call(ctx, scope, agent.ToolCall{RunID: o.RunID, StepID: dispatchDeviceStep, Tool: tool, Args: raw,
		CallSeq: seq, Epoch: epoch})
	if err != nil {
		return fmt.Errorf("service: %s order %d: %w", tool, o.OrderID, err)
	}
	if res.Status != model.ToolStatusOK {
		return fmt.Errorf("%w: %s order %d: %s %s", ErrDeviceUnavailable, tool, o.OrderID, res.Status, res.ErrorCode)
	}
	if err := json.Unmarshal(res.Data, out); err != nil {
		return fmt.Errorf("%w: %s order %d: bad data: %w", ErrDeviceUnavailable, tool, o.OrderID, err)
	}
	return nil
}

// payloadDeviceID 取 payload.device_id；缺失 / 非字符串 / payload 不可解析均返回空串（调用方判未绑定）。
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
	id, _ := p.DeviceID.(string)
	return id
}

// deviceUnavailable 是 DEVICE_UNAVAILABLE 审计的 summary；GaveUp 为 true 表示等待超限、本单已放弃下发（转 To）。
type deviceUnavailable struct {
	OrderID    uint64                `json:"order_id"`
	TicketID   uint64                `json:"ticket_id"`
	Attempt    int                   `json:"attempt"`
	DeviceType string                `json:"device_type"`
	Reason     string                `json:"reason"`
	GaveUp     bool                  `json:"gave_up,omitempty"`
	WaitSince  *time.Time            `json:"wait_since,omitempty"`
	To         model.WorkOrderStatus `json:"to,omitempty"`
}

// bindDevice 是第 ③ 步：选定设备并持 lock:order 同事务写 work_order.payload.device_id（row_ver 守卫），返回绑定后的单。
// 已绑定同一台设备时不写库。DRY_RUN 单的 device_type 无设备类别时跳过绑定。选不到：
//   - 未超限 → 审计 DEVICE_UNAVAILABLE（system、DENIED）并返回包装 ErrOutboxDeferred 的错误（outbox 推后 5s、不计 attempts，单状态不变）；
//   - 超限（deviceWaitOver）→ giveUpUnavailable 放弃下发，返回 nil（轮询器置 outbox DONE），不再推后。
//
// 每次推后重投都会在 Dispatch 里重新分配 resume_epoch（RecheckEpoch），这里有意保留：device.status 是时效工具，
// call_key 含 epoch，epoch 不变时 gateway 按 call_key 回放上一次的 offline / busy 结果，设备恢复后也永远选不到；
// ④ 复检同样要求新 epoch。递增次数由等待上限约束（2h / 5s 推后间隔 ≈ 1440 次），不再无界。
func (s *WorkOrderService) bindDevice(ctx context.Context, msg repository.OutboxMessage, o repository.DispatchOrder,
	m dispatchMessage, epoch int) (repository.DispatchOrder, error) {
	d := s.dispatch
	id, err := d.Picker.PickDevice(ctx, o.WorkOrder(), epoch)
	switch {
	case errors.Is(err, ErrDeviceNotBindable) && model.DispatchMode(o.DispatchMode) == model.DispatchDryRun:
		d.Logger.InfoContext(ctx, "dispatch: no bindable device, dry-run without device_id", "order_id", o.ID,
			"device_type", o.DeviceType)
		return o, nil
	case err == nil && id == "":
		err = fmt.Errorf("%w: picker returned empty device_id", ErrDeviceUnavailable)
	}
	if errors.Is(err, ErrDeviceUnavailable) || errors.Is(err, ErrDeviceNotBindable) {
		if s.deviceWaitOver(msg, o) {
			if gerr := s.giveUpUnavailable(ctx, msg, o, m, err); gerr != nil {
				return o, gerr
			}
			return o, errDeviceGaveUp
		}
		return o, s.deferUnavailable(ctx, o, m, err)
	}
	if err != nil {
		return o, fmt.Errorf("service: dispatch pick device order %d: %w", o.ID, err)
	}
	if payloadDeviceID(o.Payload) == id {
		return o, nil
	}
	ref := o.Ref()
	err = WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, o.ID, func(lctx context.Context) error {
		return d.Tx.InTx(lctx, func(tx *repository.DB) error {
			cur, err := d.Store.OrderForDispatch(lctx, tx, ref)
			if err != nil {
				return err
			}
			if cur.Status != o.Status {
				return fmt.Errorf("%w: order %d changed to %s before device bind", ErrDispatchState, o.ID, cur.Status)
			}
			return d.Store.BindDeviceTx(lctx, tx, ref, id, cur.RowVer, s.clock.Now())
		})
	})
	if err != nil {
		return o, fmt.Errorf("service: dispatch bind device order %d: %w", o.ID, err)
	}
	d.Logger.InfoContext(ctx, "dispatch: device bound", "order_id", o.ID, "device_id", id, "device_type", o.DeviceType)
	bound, err := d.Store.OrderForDispatch(ctx, nil, ref)
	if err != nil {
		return o, fmt.Errorf("service: dispatch: %w", err)
	}
	return bound, nil
}

// deferUnavailable 写 DEVICE_UNAVAILABLE 审计（DENIED）并返回推后错误（离线通知在 L6）。
func (s *WorkOrderService) deferUnavailable(ctx context.Context, o repository.DispatchOrder, m dispatchMessage, cause error) error {
	d := s.dispatch
	rec, err := s.unavailableAudit(o, m, cause, model.AuditResultDenied, deviceUnavailable{})
	if err != nil {
		return err
	}
	if err := d.Tx.InTx(ctx, func(tx *repository.DB) error { return d.Audit.AppendTx(ctx, tx, rec) }); err != nil {
		return fmt.Errorf("service: dispatch audit device unavailable order %d: %w", o.ID, err)
	}
	d.Logger.WarnContext(ctx, "dispatch: device unavailable, deferred", "order_id", o.ID, "device_type", o.DeviceType,
		"attempt", m.Attempt, "err", cause)
	return fmt.Errorf("%w: order %d: %w", ErrOutboxDeferred, o.ID, cause)
}

// errDeviceGaveUp 表示第 ③ 步等待超限、本单已放弃下发并落库；Dispatch 收到后返回 nil（outbox 置 DONE）。
var errDeviceGaveUp = errors.New("service: device wait exceeded, dispatch given up")

// deviceWaitOver 判断选不到设备的等待是否超限：单有 dispatch_deadline_at 时以它为准（到点即超限）；
// 没有时（手动 REAL 单等）自消息入队（outbox.created_at，推后不改）起超过 DeviceWaitMax 即超限。
// CreatedAt 为零值（未经 Claim 读回的消息）时无从起算，按未超限处理；Claim 总会读回该列（NOT NULL）。
func (s *WorkOrderService) deviceWaitOver(msg repository.OutboxMessage, o repository.DispatchOrder) bool {
	if o.DispatchDeadlineAt != nil {
		return s.overdue(o)
	}
	return !msg.CreatedAt.IsZero() && s.clock.Now().Sub(msg.CreatedAt) >= s.dispatch.DeviceWaitMax
}

// giveUpUnavailable 持 lock:order 同事务按迁移表把单转终态并写 DEVICE_UNAVAILABLE 审计（system、FAILED、gave_up=true）：
// 首次（APPROVED）→ EXPIRED（APPROVED 不能直接 → FAILED，EXPIRED 是"未在时限内下发"的现有终态）；
// 重试（DISPATCHED）→ FAILED。reason_code 都是 DEVICE_UNAVAILABLE。锁内重读时已是终态（并发中止 / 时效扫描）→ 不再动。
func (s *WorkOrderService) giveUpUnavailable(ctx context.Context, msg repository.OutboxMessage, o repository.DispatchOrder,
	m dispatchMessage, cause error) error {
	d := s.dispatch
	ref := o.Ref()
	var to model.WorkOrderStatus
	err := WithOrderLock(ctx, d.Locks, d.Keys, d.Logger, o.ID, func(lctx context.Context) error {
		return d.Tx.InTx(lctx, func(tx *repository.DB) error {
			cur, err := d.Store.OrderForDispatch(lctx, tx, ref)
			if err != nil {
				return err
			}
			from := model.WorkOrderStatus(cur.Status)
			switch {
			case from.IsTerminal():
				return nil
			case from == model.OrderApproved && m.Attempt == 1:
				to = model.OrderExpired
			case from == model.OrderDispatched && m.Attempt > 1:
				to = model.OrderFailed
			default:
				return fmt.Errorf("%w: order %d is %s at device give-up (attempt %d)", ErrDispatchState, o.ID, cur.Status, m.Attempt)
			}
			rec, err := s.unavailableAudit(o, m, cause, model.AuditResultFailed,
				deviceUnavailable{GaveUp: true, WaitSince: waitSince(msg, o), To: to})
			if err != nil {
				return err
			}
			if err := s.TransitionFrom(lctx, tx, ref, from, to, model.Actor{Type: model.ActorSystem},
				model.ReasonDeviceUnavailable); err != nil {
				return err
			}
			return d.Audit.AppendTx(lctx, tx, rec)
		})
	})
	if err != nil {
		return fmt.Errorf("service: dispatch give up order %d: %w", o.ID, err)
	}
	d.Logger.ErrorContext(ctx, "dispatch: device unavailable too long, gave up", "alert", string(model.AuditDeviceUnavailable),
		"order_id", o.ID, "attempt", m.Attempt, "to", to, "err", cause)
	return nil
}

// waitSince 是放弃时审计记录的等待起点：有 dispatch_deadline_at 时不记（以时效为准），否则记消息入队时刻。
func waitSince(msg repository.OutboxMessage, o repository.DispatchOrder) *time.Time {
	if o.DispatchDeadlineAt != nil || msg.CreatedAt.IsZero() {
		return nil
	}
	t := msg.CreatedAt
	return &t
}

// unavailableAudit 组 DEVICE_UNAVAILABLE 审计行；extra 只取 GaveUp / WaitSince / To，其余字段由 o / m / cause 填。
func (s *WorkOrderService) unavailableAudit(o repository.DispatchOrder, m dispatchMessage, cause error, result string,
	extra deviceUnavailable) (model.AuditRecord, error) {
	extra.OrderID, extra.TicketID, extra.Attempt, extra.DeviceType, extra.Reason = o.ID, m.TicketID, m.Attempt, o.DeviceType,
		cause.Error()
	payload, err := json.Marshal(extra)
	if err != nil {
		return model.AuditRecord{}, fmt.Errorf("service: encode DEVICE_UNAVAILABLE order %d: %w", o.ID, err)
	}
	runID := o.RunID
	return repository.NewAuditRecord(model.Scope{TenantID: o.TenantID, FarmID: o.FarmID}, &runID,
		model.Actor{Type: model.ActorSystem}, model.AuditDeviceUnavailable, "order:"+strconv.FormatUint(o.ID, 10), payload,
		result, s.clock.Now()), nil
}
