package model

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestFlagScopeOf(t *testing.T) {
	tests := []struct {
		tenant, farm uint64
		want         FlagScopeType
		wantErr      bool
	}{
		{0, 0, FlagScopeGlobal, false},
		{1, 0, FlagScopeTenant, false},
		{1, 2, FlagScopeFarm, false},
		{0, 2, "", true},
	}
	for _, tt := range tests {
		got, err := FlagScopeOf(tt.tenant, tt.farm)
		if got != tt.want || (err != nil) != tt.wantErr || (err != nil && !errors.Is(err, ErrFlagScope)) {
			t.Errorf("FlagScopeOf(%d,%d) = %q, %v", tt.tenant, tt.farm, got, err)
		}
	}
}

// 三种形态 × 三层：GLOBAL / TENANT 只认 force_off，FARM 按注册表形态；其余一律拒绝。
func TestParseFlagValue(t *testing.T) {
	boolSpec, modeSpec := FlagSpec{Kind: FlagKindBool}, FlagSpec{Kind: FlagKindMode}
	tests := []struct {
		name string
		spec FlagSpec
		st   FlagScopeType
		raw  string
		want FlagMode // 仅 FARM 行有意义
		ok   bool
	}{
		{"farm bool on", boolSpec, FlagScopeFarm, `{"on":true}`, FlagModeOn, true},
		{"farm bool off", boolSpec, FlagScopeFarm, `{"on":false}`, FlagModeOff, true},
		{"farm mode shadow", modeSpec, FlagScopeFarm, `{"mode":"shadow"}`, FlagModeShadow, true},
		{"global force_off", boolSpec, FlagScopeGlobal, `{"force_off":true}`, "", true},
		{"tenant force_off false", modeSpec, FlagScopeTenant, `{"force_off":false}`, "", true},
		{"global rejects on", boolSpec, FlagScopeGlobal, `{"on":true}`, "", false},
		{"tenant rejects mode", modeSpec, FlagScopeTenant, `{"mode":"on"}`, "", false},
		{"farm rejects force_off", boolSpec, FlagScopeFarm, `{"force_off":true}`, "", false},
		{"bool farm rejects mode", boolSpec, FlagScopeFarm, `{"mode":"on"}`, "", false},
		{"mode farm rejects on", modeSpec, FlagScopeFarm, `{"on":true}`, "", false},
		{"bad mode", modeSpec, FlagScopeFarm, `{"mode":"half"}`, "", false},
		{"two fields", boolSpec, FlagScopeFarm, `{"on":true,"force_off":false}`, "", false},
		{"unknown field", boolSpec, FlagScopeFarm, `{"on":true,"why":1}`, "", false},
		{"null", boolSpec, FlagScopeFarm, `{"on":null}`, "", false},
		{"string bool", boolSpec, FlagScopeFarm, `{"on":"true"}`, "", false},
		{"empty object", boolSpec, FlagScopeFarm, `{}`, "", false},
		{"not object", boolSpec, FlagScopeFarm, `true`, "", false},
		{"trailing data", boolSpec, FlagScopeFarm, `{"on":true}{"on":false}`, "", false},
		{"empty", boolSpec, FlagScopeFarm, ``, "", false},
		{"bad scope", boolSpec, "PLATFORM", `{"force_off":true}`, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseFlagValue(tt.spec, tt.st, json.RawMessage(tt.raw))
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%t", err, tt.ok)
			}
			if !tt.ok {
				if !errors.Is(err, ErrFlagValue) {
					t.Errorf("err = %v, want ErrFlagValue", err)
				}
				return
			}
			if tt.st == FlagScopeFarm && v.FarmMode() != tt.want {
				t.Errorf("FarmMode = %s, want %s", v.FarmMode(), tt.want)
			}
		})
	}
}

// 注册表：P1 两个开关与 P2 的 enable_loader_v1 / enable_compaction 为 bool，rule_offload.* 为 mode，全部默认关。
func TestDefaultFlagRegistry(t *testing.T) {
	r := DefaultFlagRegistry()
	for _, k := range []FlagKey{FlagEnableAdvice, FlagEnableRobotDispatch, FlagEnableLoaderV1, FlagEnableCompaction} {
		if s, ok := r[k]; !ok || s.Kind != FlagKindBool || s.Default != FlagModeOff {
			t.Errorf("%s = %+v ok=%t", k, s, ok)
		}
	}
	for _, k := range RuleOffloadFlags() {
		if s, ok := r[k]; !ok || s.Kind != FlagKindMode || s.Default != FlagModeOff {
			t.Errorf("%s = %+v ok=%t", k, s, ok)
		}
	}
	if len(r) != 7 {
		t.Errorf("registry has %d keys, want 7", len(r))
	}
}
