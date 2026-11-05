package agent

import (
	"context"
	"errors"
	"testing"

	"agri-steward/internal/model"
)

type fakeFlags struct {
	on   bool
	mode model.FlagMode
	err  error
}

func (f fakeFlags) Bool(context.Context, model.FlagKey, model.Scope) (bool, error) {
	return f.on, f.err
}
func (f fakeFlags) Mode(context.Context, model.FlagKey, model.Scope) (model.FlagMode, error) {
	return f.mode, f.err
}

// P2 开关读取失败关闭：未装配、读取出错、非法档位都按 off。
func TestFlagHelpers_FailClosed(t *testing.T) {
	ctx, scope := context.Background(), model.Scope{TenantID: 1, FarmID: 1}
	boom := errors.New("boom")
	if m, err := FlagModeOf(ctx, nil, model.FlagRuleOffloadTankMix, scope); m != model.FlagModeOff || !IsNoFlagReader(err) {
		t.Errorf("nil reader mode = %s, %v", m, err)
	}
	if on, err := FlagOn(ctx, nil, model.FlagEnableCompaction, scope); on || !IsNoFlagReader(err) {
		t.Errorf("nil reader bool = %t, %v", on, err)
	}
	if m, err := FlagModeOf(ctx, fakeFlags{mode: model.FlagModeOn, err: boom}, model.FlagRuleOffloadTankMix, scope); m != model.FlagModeOff || !errors.Is(err, boom) {
		t.Errorf("error mode = %s, %v", m, err)
	}
	if on, err := FlagOn(ctx, fakeFlags{on: true, err: boom}, model.FlagEnableCompaction, scope); on || !errors.Is(err, boom) {
		t.Errorf("error bool = %t, %v", on, err)
	}
	if m, err := FlagModeOf(ctx, fakeFlags{mode: "bogus"}, model.FlagRuleOffloadTankMix, scope); m != model.FlagModeOff || !errors.Is(err, model.ErrFlagValue) {
		t.Errorf("bogus mode = %s, %v", m, err)
	}
	if m, err := FlagModeOf(ctx, fakeFlags{mode: model.FlagModeShadow}, model.FlagRuleOffloadTankMix, scope); m != model.FlagModeShadow || err != nil {
		t.Errorf("shadow mode = %s, %v", m, err)
	}
	if on, err := FlagOn(ctx, fakeFlags{on: true}, model.FlagEnableCompaction, scope); !on || err != nil || IsNoFlagReader(err) {
		t.Errorf("on = %t, %v", on, err)
	}
}
