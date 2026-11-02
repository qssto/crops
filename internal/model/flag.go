package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FlagKey 是 feature_flag.flag_key（P1 §6.1）；只认注册表内的 key。
type FlagKey string

const (
	FlagEnableAdvice        FlagKey = "enable_advice"
	FlagEnableRobotDispatch FlagKey = "enable_robot_dispatch"
)

// FlagKind 是注册表声明的 FARM 行值形态。
type FlagKind string

const (
	FlagKindBool FlagKind = "bool" // {"on":bool}
	FlagKindMode FlagKind = "mode" // {"mode":"off|shadow|on"}
)

// FlagMode 是 mode 形态的取值；bool 形态合并后按 on / off 表示。
type FlagMode string

const (
	FlagModeOff    FlagMode = "off"
	FlagModeShadow FlagMode = "shadow"
	FlagModeOn     FlagMode = "on"
)

// FlagScopeType 是 feature_flag.scope_type，与 (tenant_id, farm_id) 是否为 0 一致。
type FlagScopeType string

const (
	FlagScopeGlobal FlagScopeType = "GLOBAL" // (0,0)
	FlagScopeTenant FlagScopeType = "TENANT" // (t,0)
	FlagScopeFarm   FlagScopeType = "FARM"   // (t,f)
)

// 开关取值错误。
var (
	ErrFlagScope = errors.New("model: bad flag scope")
	ErrFlagValue = errors.New("model: bad flag value")
)

// FlagScopeOf 由 (tenant_id, farm_id) 推出 scope_type；(0,f) 非法。
func FlagScopeOf(tenantID, farmID uint64) (FlagScopeType, error) {
	switch {
	case tenantID == 0 && farmID == 0:
		return FlagScopeGlobal, nil
	case farmID == 0:
		return FlagScopeTenant, nil
	case tenantID != 0:
		return FlagScopeFarm, nil
	}
	return "", fmt.Errorf("%w: tenant_id=0 farm_id=%d", ErrFlagScope, farmID)
}

// FlagSpec 是注册表一项：FARM 行形态与没有 FARM 行时的默认值。
type FlagSpec struct {
	Kind    FlagKind
	Default FlagMode
}

// FlagRegistry 是 flag_key → FlagSpec；未注册的 key 一律拒绝。
type FlagRegistry map[FlagKey]FlagSpec

// DefaultFlagRegistry 是 P1 注册的开关，默认全关（P1 §6.1 功能任务 2）。
func DefaultFlagRegistry() FlagRegistry {
	return FlagRegistry{
		FlagEnableAdvice:        {Kind: FlagKindBool, Default: FlagModeOff},
		FlagEnableRobotDispatch: {Kind: FlagKindBool, Default: FlagModeOff},
	}
}

// FlagValue 是 feature_flag.value 的三种形态（母文档 §4.6.2-5），恰有一个字段非空。
type FlagValue struct {
	On       *bool     `json:"on,omitempty"`
	Mode     *FlagMode `json:"mode,omitempty"`
	ForceOff *bool     `json:"force_off,omitempty"`
}

// ParseFlagValue 严格解析并校验一行取值：GLOBAL / TENANT 只接受 force_off，FARM 按 spec.Kind 只接受 on 或 mode；
// 未知字段、null、多字段、尾随数据都拒绝。
func ParseFlagValue(spec FlagSpec, st FlagScopeType, raw json.RawMessage) (FlagValue, error) {
	var v FlagValue
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return FlagValue{}, fmt.Errorf("%w: %v", ErrFlagValue, err)
	}
	if dec.More() {
		return FlagValue{}, fmt.Errorf("%w: trailing data", ErrFlagValue)
	}
	var want string
	ok := false
	switch {
	case st == FlagScopeGlobal || st == FlagScopeTenant:
		want, ok = `{"force_off":bool}`, v.ForceOff != nil && v.On == nil && v.Mode == nil
	case st == FlagScopeFarm && spec.Kind == FlagKindBool:
		want, ok = `{"on":bool}`, v.On != nil && v.Mode == nil && v.ForceOff == nil
	case st == FlagScopeFarm && spec.Kind == FlagKindMode:
		want = `{"mode":"off|shadow|on"}`
		ok = v.Mode != nil && v.On == nil && v.ForceOff == nil && v.Mode.Valid()
	default:
		return FlagValue{}, fmt.Errorf("%w: scope %q kind %q", ErrFlagValue, st, spec.Kind)
	}
	if !ok {
		return FlagValue{}, fmt.Errorf("%w: %s row of %s flag wants %s, got %s", ErrFlagValue, st, spec.Kind, want, raw)
	}
	return v, nil
}

// Valid 判断 mode 取值合法。
func (m FlagMode) Valid() bool {
	return m == FlagModeOff || m == FlagModeShadow || m == FlagModeOn
}

// FarmMode 返回 FARM 行的生效档位（bool 形态 on → on，否则 off）。
func (v FlagValue) FarmMode() FlagMode {
	switch {
	case v.Mode != nil:
		return *v.Mode
	case v.On != nil && *v.On:
		return FlagModeOn
	}
	return FlagModeOff
}

// FlagRow 是三层读取的一行（scope_type, value）。
type FlagRow struct {
	ScopeType FlagScopeType
	Value     json.RawMessage
}

// FlagWrite 是一次开关写入（upsert 或乐观锁更新）。
type FlagWrite struct {
	TenantID  uint64
	FarmID    uint64
	Key       FlagKey
	ScopeType FlagScopeType
	Value     json.RawMessage
	Comment   string
	UpdatedBy uint64
	At        time.Time
}

// FlagSetting 是灰度档位一览的一行（P1 §6.9 末尾 SQL）。
type FlagSetting struct {
	TenantID  uint64          `json:"tenant_id"`
	FarmID    uint64          `json:"farm_id"`
	ScopeType FlagScopeType   `json:"scope_type"`
	Value     json.RawMessage `json:"value"`
	Comment   string          `json:"comment"`
	RowVer    int             `json:"row_ver"`
	UpdatedAt time.Time       `json:"updated_at"`
}
