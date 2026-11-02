package adapter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"agri-steward/internal/model"
)

// ErrRegistry 表示注册失败（启动自检：app 任一注册失败即启动失败，P1 §6.3 验收 2）。
var ErrRegistry = errors.New("adapter: registry")

// CheckCapabilities 是开放规则（P1 §6.1 功能任务 5）：必须支持 Cancel，且幂等键与外部单号查询至少一项。
func CheckCapabilities(c Capabilities) error {
	switch {
	case !c.Cancel:
		return fmt.Errorf("%w: cancel not supported", ErrCapability)
	case !c.IdemKey && !c.ExternalQuery:
		return fmt.Errorf("%w: neither idem_key nor external query supported", ErrCapability)
	}
	return nil
}

// Entry 是注册表的一行（启动日志与设备开放清单用）。
type Entry struct {
	DeviceType string
	Open       bool
	Reason     string // 不可开放原因；可开放为空
	Caps       Capabilities
}

// Registry 按 work_order.device_type 登记 Adapter。可开放集合只含通过 CheckCapabilities 的实现；
// RegisterClosed 登记的骨架类型不进可开放集合，Get 返回 ErrDeviceNotOpen。启动时装配完成后只读，并发安全。
type Registry struct {
	mu     sync.RWMutex
	open   map[string]DeviceAdapter
	closed map[string]string
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{open: map[string]DeviceAdapter{}, closed: map[string]string{}}
}

// Register 登记可开放的设备类型；能力不满足开放规则、类型为空、Adapter 为 nil（含 typed nil）或重复登记均返回错误。
func (r *Registry) Register(deviceType string, a DeviceAdapter) error {
	if deviceType == "" || isNil(a) {
		return fmt.Errorf("%w: device type and adapter are required", ErrRegistry)
	}
	if err := CheckCapabilities(a.Capabilities()); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrRegistry, deviceType, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.freeLocked(deviceType); err != nil {
		return err
	}
	r.open[deviceType] = a
	return nil
}

// RegisterClosed 登记不可开放的设备类型（骨架 / 协议未定）；reason 写入启动日志与 ErrDeviceNotOpen。
func (r *Registry) RegisterClosed(deviceType, reason string) error {
	if deviceType == "" || reason == "" {
		return fmt.Errorf("%w: device type and reason are required", ErrRegistry)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.freeLocked(deviceType); err != nil {
		return err
	}
	r.closed[deviceType] = reason
	return nil
}

func (r *Registry) freeLocked(deviceType string) error {
	_, o := r.open[deviceType]
	_, c := r.closed[deviceType]
	if o || c {
		return fmt.Errorf("%w: device type %q registered twice", ErrRegistry, deviceType)
	}
	return nil
}

// isNil 判断 Adapter 是否为 nil 接口或装着 nil 指针 / map / func 等的 typed nil
// （如 var c *devicesim.Client; Register("uav", c)），避免在 Capabilities() 处 panic。
func isNil(a DeviceAdapter) bool {
	if a == nil {
		return true
	}
	switch v := reflect.ValueOf(a); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// Has 判断类型是否已登记（可开放或不可开放）。
func (r *Registry) Has(deviceType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, o := r.open[deviceType]
	_, c := r.closed[deviceType]
	return o || c
}

// Openable 判断类型是否在可开放集合中（to_order 写 REAL 的条件之一）。
func (r *Registry) Openable(deviceType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.open[deviceType]
	return ok
}

// Get 返回可开放类型的 Adapter；未登记或不可开放返回 ErrDeviceNotOpen。
func (r *Registry) Get(deviceType string) (DeviceAdapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.open[deviceType]; ok {
		return a, nil
	}
	if reason, ok := r.closed[deviceType]; ok {
		return nil, fmt.Errorf("%w: %q: %s", ErrDeviceNotOpen, deviceType, reason)
	}
	return nil, fmt.Errorf("%w: %q not registered", ErrDeviceNotOpen, deviceType)
}

// Entries 返回全部登记（按类型升序）。
func (r *Registry) Entries() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.open)+len(r.closed))
	for t, a := range r.open {
		out = append(out, Entry{DeviceType: t, Open: true, Caps: a.Capabilities()})
	}
	for t, reason := range r.closed {
		out = append(out, Entry{DeviceType: t, Reason: reason})
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(a.DeviceType, b.DeviceType) })
	return out
}

// Skeleton 是未对接协议的真实设备骨架（D4 协议未提供）：能力全未声明，调用一律返回 ErrDeviceNotOpen。
// 只作为 RegisterClosed 的登记依据与日后对接的落点，不进可开放集合。
type Skeleton struct {
	DeviceType string
	Family     string // 设备系列，如「植保 P 系列」
}

var _ DeviceAdapter = Skeleton{}

// Capabilities 实现 DeviceAdapter：协议未定，全部未声明。
func (Skeleton) Capabilities() Capabilities { return Capabilities{} }

// Dispatch 实现 DeviceAdapter。
func (s Skeleton) Dispatch(context.Context, model.WorkOrder, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, s.notOpen()
}

// QueryStatus 实现 DeviceAdapter。
func (s Skeleton) QueryStatus(context.Context, StatusRef) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, s.notOpen()
}

// Cancel 实现 DeviceAdapter。
func (s Skeleton) Cancel(context.Context, string) (model.DeviceReceipt, error) {
	return model.DeviceReceipt{}, s.notOpen()
}

func (s Skeleton) notOpen() error {
	return fmt.Errorf("%w: %s (%s) protocol not provided (D4)", ErrDeviceNotOpen, s.DeviceType, s.Family)
}

// 四类真实设备骨架的 device_type（与 work_order.device_type 一致；uav / valve 由 to_order 产出）。
const (
	DeviceTypeUAV         = "uav"         // 植保 P 系列
	DeviceTypeVehicle     = "vehicle"     // 车辆 R·RM 系列
	DeviceTypeValve       = "valve"       // 阀门 FBV·FGV
	DeviceTypeFertigation = "fertigation" // 智能施肥 FPDI·FPI
)

// Skeletons 是四类真实设备骨架（《设备开放清单》：本阶段不开放）。
func Skeletons() []Skeleton {
	return []Skeleton{
		{DeviceType: DeviceTypeUAV, Family: "植保 P 系列"},
		{DeviceType: DeviceTypeVehicle, Family: "车辆 R·RM 系列"},
		{DeviceType: DeviceTypeValve, Family: "阀门 FBV·FGV"},
		{DeviceType: DeviceTypeFertigation, Family: "智能施肥 FPDI·FPI"},
	}
}

// RegisterSkeletons 把骨架登记为不可开放；已登记的类型（如预发把 uav 指向 devicesim）跳过。
func RegisterSkeletons(r *Registry) error {
	for _, s := range Skeletons() {
		if r.Has(s.DeviceType) {
			continue
		}
		if err := r.RegisterClosed(s.DeviceType, s.Family+": 协议未提供（D4），本阶段不开放"); err != nil {
			return err
		}
	}
	return nil
}
