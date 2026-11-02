package constraint

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/model"
)

type stubChecker string

func (s stubChecker) RuleID() string      { return string(s) }
func (s stubChecker) Check(*Input) Result { return Result{Passed: true} }

func TestNewRegistry_Rejects(t *testing.T) {
	for name, cs := range map[string][]Checker{
		"duplicate": {stubChecker("a"), stubChecker("a")},
		"empty":     {stubChecker("")},
		"too long":  {stubChecker(strings.Repeat("x", RuleIDMax+1))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistry(cs...); !errors.Is(err, ErrBadRegistry) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRegistry_Required(t *testing.T) {
	r := Default()
	cases := []struct {
		phase  model.ConstraintPhase
		wt     model.WorkType
		device string
		want   []string
	}{
		{model.PhasePreApproval, model.WorkSpray, DeviceKnapsack,
			[]string{RuleRegistered, RuleWind, RuleRain, RuleTemp, RulePHI, RuleRepeat, RuleDrift}},
		{model.PhasePreApproval, model.WorkSpray, DeviceUAV,
			[]string{RuleRegistered, RuleWind, RuleRain, RuleTemp, RulePHI, RuleRepeat, RuleNoFly, RuleDrift}},
		{model.PhasePreDispatch, model.WorkSpray, DeviceUAV,
			[]string{RuleRegistered, RuleWind, RuleRain, RuleTemp, RulePHI, RuleRepeat, RuleNoFly, RuleDrift, RuleBattery, RulePayload}},
		{model.PhasePreApproval, model.WorkFertilize, DeviceKnapsack, []string{RuleWind, RuleRain, RuleTemp, RuleRepeat}},
		{model.PhasePreApproval, model.WorkFertilize, DeviceManual, []string{RuleRain, RuleRepeat}},
		{model.PhasePreApproval, model.WorkIrrigate, DeviceValve, []string{RuleRepeat}},
		{model.PhasePreApproval, "PRUNE", DeviceManual, nil},
	}
	for _, c := range cases {
		if got := r.Required(c.phase, c.wt, c.device); !slices.Equal(got, c.want) {
			t.Errorf("%s %s %s: got %v, want %v", c.phase, c.wt, c.device, got, c.want)
		}
	}
}

func TestRegistry_Evaluate(t *testing.T) {
	t.Run("empty plan blocks", func(t *testing.T) {
		rows := Default().Evaluate(input(t, work("PRUNE")))
		if len(rows) != 1 || rows[0].RuleID != RuleNoPlan || rows[0].Passed {
			t.Fatalf("rows = %+v", rows)
		}
	})
	t.Run("required but unregistered fails", func(t *testing.T) {
		r, err := NewRegistry(repeatChecker{})
		if err != nil {
			t.Fatal(err)
		}
		rows := r.Evaluate(input(t, work(model.WorkIrrigate), with(history(map[string]any{"operations": []any{}}))))
		if len(rows) != 1 || !rows[0].Passed {
			t.Fatalf("irrigate rows = %+v", rows)
		}
		rows = r.Evaluate(input(t, work(model.WorkFertilize), device(DeviceManual), with(history(map[string]any{}))))
		if len(rows) != 2 || rows[1].RuleID != RuleRain || rows[1].Passed { // 未注册的必检项排在最后
			t.Fatalf("fertilize rows = %+v", rows)
		}
	})
	t.Run("rows carry phase and json", func(t *testing.T) {
		rows := Default().Evaluate(input(t, func(in *Input) { in.Phase = model.PhasePreDispatch }))
		for _, c := range rows {
			if c.Phase != model.PhasePreDispatch || !json.Valid(c.Observed) || !json.Valid(c.Threshold) {
				t.Errorf("row %+v", c)
			}
			if c.Passed && c.RuleID != RuleRegistered {
				t.Errorf("%s passed without any data", c.RuleID)
			}
		}
	})
}

func TestParseRegulationMap(t *testing.T) {
	m := repoRegulation(t)
	if len(m.Entries()) == 0 {
		t.Fatal("repo regulation map empty")
	}
	for _, e := range m.Entries() {
		if e.LegalBasis != "待法务填写" {
			t.Errorf("%s legal_basis = %q", e.ID, e.LegalBasis)
		}
	}
	const head = "version: 1\nentries:\n"
	bad := map[string]string{
		"version":           "version: 2\nentries: []\n",
		"unknown field":     head + "  - {id: a, title: t, kind: checker, rule_id: wind_speed_max, legal_basis: x, extra: 1}\n",
		"unregistered rule": head + "  - {id: a, title: t, kind: checker, rule_id: nope, legal_basis: x}\n",
		"kind mismatch":     head + "  - {id: a, title: t, kind: checker, rule_id: reg_pesticide_registered, legal_basis: x}\n",
		"unmapped rule_id":  head + "  - {id: a, title: t, kind: unmapped, rule_id: wind_speed_max, legal_basis: x}\n",
		"bad kind":          head + "  - {id: a, title: t, kind: maybe, legal_basis: x}\n",
		"duplicate":         head + "  - {id: a, title: t, kind: unmapped, legal_basis: x}\n  - {id: a, title: t, kind: unmapped, legal_basis: x}\n",
		"no legal_basis":    head + "  - {id: a, title: t, kind: unmapped}\n",
		"unknown device":    head + "  - {id: a, title: t, kind: unmapped, legal_basis: x, applies_to: {device_types: [jet]}}\n",
		"id too long":       head + "  - {id: " + strings.Repeat("a", RuleIDMax) + ", title: t, kind: unmapped, legal_basis: x}\n",
	}
	for name, src := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRegulationMap([]byte(src), Default()); !errors.Is(err, ErrBadRegulationMap) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// passRow 是带完整证据的通过行。
func passRow(id string) model.ConstraintCheck {
	return Row(model.PhasePreApproval, id, Result{Passed: true,
		Observed:  Observed{Value: 1.0, Source: "fixture", ToolCallID: 3},
		Threshold: Threshold{KB: "kb:x@v1", ThresholdID: "t", Op: ">=", Value: 0}})
}

func allPass(ids ...string) []model.ConstraintCheck {
	out := make([]model.ConstraintCheck, 0, len(ids))
	for _, id := range ids {
		out = append(out, passRow(id))
	}
	return out
}

func TestReviewer_Review(t *testing.T) {
	reg := Default()
	rv := NewReviewer(reg, repoRegulation(t))
	spray := model.WorkOrder{OrderID: 1, WorkType: model.WorkSpray, DeviceType: DeviceKnapsack, DispatchMode: model.DispatchDryRun}
	full := allPass(reg.Required(model.PhasePreApproval, model.WorkSpray, DeviceKnapsack)...)

	t.Run("all pass", func(t *testing.T) {
		o := spray
		o.ConstraintChecks = append(full, passRow("common/x#legacy")) // 注册表外的行不产生影响
		rows, blocked := rv.Review(o)
		if len(blocked) != 0 || len(rows) != len(full) || rows[0].RuleID != RuleRegistered {
			t.Fatalf("blocked %v rows %d first %s", blocked, len(rows), rows[0].RuleID)
		}
	})
	t.Run("missing required blocks", func(t *testing.T) {
		o := spray
		o.ConstraintChecks = full[:len(full)-1]
		if _, blocked := rv.Review(o); !slices.Equal(blocked, []string{RuleDrift}) {
			t.Fatalf("blocked %v", blocked)
		}
	})
	t.Run("passed without tool_call_id is forced to fail", func(t *testing.T) {
		o := spray
		o.ConstraintChecks = slices.Clone(full)
		o.ConstraintChecks[1] = Row(model.PhasePreApproval, RuleWind, Result{Passed: true,
			Observed: Observed{Value: 1.0, Source: "fixture"}, Threshold: Threshold{KB: "kb:x@v1", Op: "<=", Value: 3}})
		rows, blocked := rv.Review(o)
		if !slices.Equal(blocked, []string{RuleWind}) || !strings.Contains(string(rows[1].Observed), "tool_call_id") {
			t.Fatalf("blocked %v row %s", blocked, rows[1].Observed)
		}
	})
	t.Run("precondition exempt from tool_call_id, ordered first", func(t *testing.T) {
		o := spray
		o.ConstraintChecks = slices.Clone(full[1:])
		o.ConstraintChecks = append(o.ConstraintChecks, Row(model.PhasePreApproval, RuleRegistered, Result{Passed: false,
			Observed: Observed{Source: "pesticide_registry"}, Threshold: Threshold{KB: "kb:common/pesticide_registry@v1"}}))
		rows, blocked := rv.Review(o)
		if !slices.Equal(blocked, []string{RuleRegistered}) || rows[0].RuleID != RuleRegistered {
			t.Fatalf("blocked %v first %s", blocked, rows[0].RuleID)
		}
	})
	t.Run("unmapped regulation blocks uav REAL", func(t *testing.T) {
		o := model.WorkOrder{WorkType: model.WorkSpray, DeviceType: DeviceUAV, DispatchMode: model.DispatchReal,
			ConstraintChecks: allPass(reg.Required(model.PhasePreApproval, model.WorkSpray, DeviceUAV)...)}
		rows, blocked := rv.Review(o)
		want := []string{UnmappedPrefix + "uav-flight-plan", UnmappedPrefix + "uav-operator-license"}
		if !slices.Equal(blocked, want) || rows[1].RuleID != want[0] {
			t.Fatalf("blocked %v, rows[1] %s", blocked, rows[1].RuleID)
		}
		o.DispatchMode = model.DispatchDryRun
		if _, blocked := rv.Review(o); len(blocked) != 0 {
			t.Fatalf("dry-run blocked %v", blocked)
		}
	})
	t.Run("empty checks block", func(t *testing.T) {
		o := spray
		o.ConstraintChecks = nil
		if _, blocked := rv.Review(o); len(blocked) != len(full) {
			t.Fatalf("blocked %v", blocked)
		}
	})
	t.Run("no plan blocks", func(t *testing.T) {
		rows, blocked := rv.Review(model.WorkOrder{WorkType: "PRUNE", DeviceType: DeviceManual})
		if !slices.Equal(blocked, []string{RuleNoPlan}) || len(rows) != 1 {
			t.Fatalf("blocked %v", blocked)
		}
	})
	t.Run("no plan blocks even with applicable regulation", func(t *testing.T) {
		regs, err := ParseRegulationMap([]byte("version: 1\nentries:\n  - {id: any, title: t, kind: unmapped, legal_basis: x}\n"), reg)
		if err != nil {
			t.Fatal(err)
		}
		_, blocked := NewReviewer(reg, regs).Review(model.WorkOrder{WorkType: "PRUNE", DeviceType: DeviceManual})
		if !slices.Equal(blocked, []string{UnmappedPrefix + "any", RuleNoPlan}) {
			t.Fatalf("blocked %v", blocked)
		}
	})
}

// TestReviewer_Recompute（审查 3）：通过行按行内 threshold.op / value 复算 observed.value，不一致或数值类规则缺值即改判失败。
func TestReviewer_Recompute(t *testing.T) {
	reg := Default()
	rv := NewReviewer(reg, repoRegulation(t))
	full := allPass(reg.Required(model.PhasePreApproval, model.WorkSpray, DeviceKnapsack)...)
	row := func(id string, v any, op string, th float64) model.ConstraintCheck {
		return Row(model.PhasePreApproval, id, Result{Passed: true, Observed: Observed{Value: v, Source: "fixture", ToolCallID: 3},
			Threshold: Threshold{KB: "kb:x@v1", ThresholdID: "t", Op: op, Value: th}})
	}
	// nilRow 是观测值为空、带说明的通过行（复核第 23 条）。
	nilRow := func(id, note, op string) model.ConstraintCheck {
		return Row(model.PhasePreApproval, id, Result{Passed: true,
			Observed: Observed{Source: "fixture", ToolCallID: 3, Note: note}, Threshold: Threshold{KB: "kb:x@v1", Op: op, Value: 20}})
	}
	cases := []struct {
		name    string
		row     model.ConstraintCheck
		blocked bool
	}{
		{"consistent wind passes", row(RuleWind, 3.0, "<=", 5.4), false},
		{"wind over threshold flagged passed", row(RuleWind, 6.0, "<=", 5.4), true},
		{"wind without value", row(RuleWind, nil, "<=", 5.4), true},
		{"wind string value", row(RuleWind, "calm", "<=", 5.4), true},
		{"unknown op", row(RuleWind, 1.0, "~", 5.4), true},
		{"phi nil value no longer ok", nilRow(RulePHI, "phi not applicable", ">="), true},
		{"phi negative margin flagged passed", row(RulePHI, -1.0, ">=", 0), true},
		{"drift no sensitive nil ok", nilRow(RuleDrift, noteNoSensitive, ">"), false},
		{"drift none downwind nil ok", nilRow(RuleDrift, noteNoDownwind, ">"), false},
		{"repeat no previous spray nil ok", nilRow(RuleRepeat, "no previous spray", ">="), false},
		{"drift nil without note", nilRow(RuleDrift, "", ">"), true},
		{"drift nil with free-form note", nilRow(RuleDrift, "looks fine", ">"), true},
		{"repeat nil unknown kind note", nilRow(RuleRepeat, "no previous prune", ">="), true},
		{"drift nil without threshold op", nilRow(RuleDrift, noteNoSensitive, ""), true},
		{"repeat nil without threshold op", nilRow(RuleRepeat, "no previous spray", ""), true},
		{"precondition bool true", Row(model.PhasePreApproval, RuleRegistered, Result{Passed: true,
			Observed: Observed{Value: true, Source: "pesticide_registry"}, Threshold: Threshold{KB: "kb:r@v1", Op: "==", Value: 1}}), false},
		{"precondition bool false flagged passed", Row(model.PhasePreApproval, RuleRegistered, Result{Passed: true,
			Observed: Observed{Value: false, Source: "pesticide_registry"}, Threshold: Threshold{KB: "kb:r@v1", Op: "==", Value: 1}}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := model.WorkOrder{WorkType: model.WorkSpray, DeviceType: DeviceKnapsack, DispatchMode: model.DispatchDryRun}
			o.ConstraintChecks = append([]model.ConstraintCheck{tc.row}, full...) // 同 rule_id 取第一行
			_, blocked := rv.Review(o)
			if got := slices.Contains(blocked, tc.row.RuleID); got != tc.blocked || len(blocked) > 1 {
				t.Fatalf("blocked %v, want %s blocked=%v", blocked, tc.row.RuleID, tc.blocked)
			}
		})
	}
}

// 复核第 23 条：无人机单的 no_fly_zone 空值通过行须带 threshold.op 且 note 为「no zone nearby」。
func TestReviewer_NilValueNoFly(t *testing.T) {
	reg := Default()
	rv := NewReviewer(reg, repoRegulation(t))
	full := allPass(reg.Required(model.PhasePreApproval, model.WorkSpray, DeviceUAV)...)
	for _, tc := range []struct {
		name, note, op string
		blocked        bool
	}{
		{"no zone nearby ok", noteNoZone, ">", false},
		{"free-form note", "clear sky", ">", true},
		{"without threshold op", noteNoZone, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := Row(model.PhasePreApproval, RuleNoFly, Result{Passed: true,
				Observed: Observed{Source: "fixture", ToolCallID: 3, Note: tc.note}, Threshold: Threshold{KB: "kb:x@v1", Op: tc.op, Value: 500}})
			o := model.WorkOrder{WorkType: model.WorkSpray, DeviceType: DeviceUAV, DispatchMode: model.DispatchDryRun,
				ConstraintChecks: append([]model.ConstraintCheck{row}, full...)}
			_, blocked := rv.Review(o)
			if got := slices.Contains(blocked, RuleNoFly); got != tc.blocked || len(blocked) > 1 {
				t.Fatalf("blocked %v, want no_fly_zone blocked=%v", blocked, tc.blocked)
			}
		})
	}
}
