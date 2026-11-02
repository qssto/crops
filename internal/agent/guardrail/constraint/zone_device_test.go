package constraint

import (
	"slices"
	"testing"
	"time"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/model"
)

func profile(kv ...any) Source {
	d := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i].(string)] = kv[i+1]
	}
	return ok(ToolProfile, d)
}

func zone(kind string, d float64) map[string]any {
	return map[string]any{"name": "z", "kind": kind, "distance_m": d}
}

func TestNoFlyChecker(t *testing.T) {
	uav := device(DeviceUAV)
	runCases(t, noFlyChecker{}, []checkCase{
		{name: "empty list passes", in: input(t, uav, with(profile("no_fly_zones", []any{}))), want: true},
		{name: "no-fly boundary 0 m counts as inside", in: input(t, uav, with(profile("no_fly_zones", []any{zone("no_fly", 0)}))), want: false, value: 0.0},
		{name: "no-fly 0.5 m outside", in: input(t, uav, with(profile("no_fly_zones", []any{zone("no_fly", 0.5)}))), want: true},
		{name: "sensitive boundary 100 m fails", in: input(t, uav, with(profile("no_fly_zones", []any{zone("sensitive", 100)}))), want: false},
		{name: "sensitive 100.5 m passes", in: input(t, uav, with(profile("no_fly_zones", []any{zone("sensitive", 100.5)}))), want: true},
		{name: "worst margin of several", in: input(t, uav, with(profile("no_fly_zones", []any{zone("sensitive", 150), zone("no_fly", 30)}))), want: true, value: 30.0},
		{name: "worst of several fails", in: input(t, uav, with(profile("no_fly_zones", []any{zone("sensitive", 300), zone("sensitive", 90)}))), want: false, value: 90.0},
		{name: "negative distance fails", in: input(t, uav, with(profile("no_fly_zones", []any{zone("sensitive", -5)}))), want: false},
		{name: "unknown kind fails", in: input(t, uav, with(profile("no_fly_zones", []any{zone("military", 900)}))), want: false},
		{name: "distance missing fails", in: input(t, uav, with(profile("no_fly_zones", []any{map[string]any{"kind": "no_fly"}}))), want: false},
		{name: "field missing fails", in: input(t, uav, with(profile("crop", "rice"))), want: false},
		{name: "profile not called", in: input(t, uav), want: false},
	})
}

func neighbour(bearing, d float64, sensitive bool) map[string]any {
	return map[string]any{"label": "n", "crop": "mulberry", "sensitive": sensitive, "bearing_deg": bearing, "distance_m": d}
}

func TestDriftChecker(t *testing.T) {
	adj := func(ns ...any) Source { return profile("adjacent_fields", ns) }
	wind := func(from float64) Source { return forecast("wind_dir_deg", from) }
	runCases(t, driftChecker{}, []checkCase{
		{name: "no adjacent fields", in: input(t, with(adj())), want: true},
		{name: "non-sensitive ignored, no wind needed", in: input(t, with(adj(neighbour(180, 1, false)))), want: true},
		{name: "knapsack downwind boundary 20 m fails", in: input(t, with(adj(neighbour(180, 20, true)), wind(0))), want: false, value: 20.0},
		{name: "knapsack downwind 20.5 m passes", in: input(t, with(adj(neighbour(180, 20.5, true)), wind(0))), want: true},
		{name: "uav downwind 50 m fails", in: input(t, device(DeviceUAV), with(adj(neighbour(180, 50, true)), wind(0))), want: false},
		{name: "sector edge 45 deg counts", in: input(t, with(adj(neighbour(225, 5, true)), wind(0))), want: false},
		{name: "outside sector 46 deg", in: input(t, with(adj(neighbour(226, 5, true)), wind(0))), want: true},
		{name: "upwind ignored", in: input(t, with(adj(neighbour(0, 5, true)), wind(0))), want: true},
		{name: "wrap-around wind 350", in: input(t, with(adj(neighbour(125, 5, true)), wind(350))), want: false},
		{name: "wind direction missing", in: input(t, with(adj(neighbour(180, 5, true)), forecast("wind_speed_ms", 1.0))), want: false},
		{name: "forecast missing", in: input(t, with(adj(neighbour(180, 5, true)))), want: false},
		{name: "adjacent missing", in: input(t, with(profile("crop", "rice"), wind(0))), want: false},
		{name: "sensitive missing fails", in: input(t, with(adj(map[string]any{"bearing_deg": 0.0, "distance_m": 500.0}), wind(0))), want: false},
		{name: "sensitive string fails", in: input(t, with(adj(map[string]any{"sensitive": "true", "bearing_deg": 0.0,
			"distance_m": 500.0}), wind(0))), want: false},
		{name: "negative distance fails", in: input(t, with(adj(neighbour(0, -1, true)), wind(0))), want: false},
		{name: "downwind on tomorrow in window fails", in: input(t, window(24), with(adj(neighbour(180, 5, true)),
			daily(dayRec(today, "wind_dir_deg", 180.0), dayRec(today.AddDate(0, 0, 1), "wind_dir_deg", 0.0)))), want: false, value: 5.0},
	})
	t.Run("sector op from knowledge", func(t *testing.T) {
		in := input(t, with(adj(neighbour(180, 5, true)), wind(0)))
		in.KB = patchedOpKB{base: repoKB(t), kbID: KBDrift, thID: thDriftSector, op: "<"} // 0° < 45° 仍在扇区
		if got := (driftChecker{}).Check(in); got.Passed {
			t.Fatalf("got %+v", got)
		}
		in = input(t, with(adj(neighbour(225, 5, true)), wind(0)))
		in.KB = patchedOpKB{base: repoKB(t), kbID: KBDrift, thID: thDriftSector, op: "<"} // 45° 不再 < 45°
		if got := (driftChecker{}).Check(in); !got.Passed {
			t.Fatalf("got %+v", got)
		}
	})
}

// patchedOpKB 改写某条阈值的 op（验证比较符取自知识）。
type patchedOpKB struct {
	base           KB
	kbID, thID, op string
}

func (p patchedOpKB) Entry(id string) (*knowledge.Entry, bool) {
	en, ok := p.base.Entry(id)
	if !ok || id != p.kbID {
		return en, ok
	}
	cp := *en
	cp.Thresholds = slices.Clone(en.Thresholds)
	for i := range cp.Thresholds {
		if cp.Thresholds[i].ID == p.thID {
			cp.Thresholds[i].Op = p.op
		}
	}
	return &cp, true
}

const testDeviceID = "dev-uav-01"

func status(kv ...any) Source {
	d := map[string]any{"device_id": testDeviceID, "online": true, "state": "idle", "battery_pct": 100.0, "tank_l": 30.0,
		"last_seen_at": testNow.Add(-time.Minute).Format(time.RFC3339)}
	for i := 0; i+1 < len(kv); i += 2 {
		d[kv[i].(string)] = kv[i+1]
	}
	return ok(ToolDevice, d)
}

func area(mu float64) func(*Input) { return func(in *Input) { in.AreaMu = f64p(mu) } }

// uavDispatch 是无人机下发前复检输入（作业单 payload.device_id = testDeviceID）。
func uavDispatch(in *Input) {
	in.DeviceType, in.Phase, in.DeviceID = DeviceUAV, model.PhasePreDispatch, testDeviceID
}

func TestBatteryChecker(t *testing.T) {
	uav := uavDispatch
	noDevice := func(in *Input) { in.DeviceID = "" }
	runCases(t, batteryChecker{}, []checkCase{
		// 10 亩 < 30 L / 1.5 L = 20 亩 → 10 / 25 = 40 %
		{name: "boundary 40% passes", in: input(t, uav, area(10), with(status("battery_pct", 40.0))), want: true, value: 40.0},
		{name: "39% fails", in: input(t, uav, area(10), with(status("battery_pct", 39.0))), want: false},
		{name: "large field capped by tank: 80%", in: input(t, uav, area(100), with(status("battery_pct", 79.0))), want: false},
		{name: "offline", in: input(t, uav, area(10), with(status("online", false))), want: false},
		{name: "fault", in: input(t, uav, area(10), with(status("state", "fault"))), want: false},
		{name: "stale last_seen_at", in: input(t, uav, area(10),
			with(status("last_seen_at", testNow.Add(-301*time.Second).Format(time.RFC3339)))), want: false},
		{name: "last_seen_at missing", in: input(t, uav, area(10), with(status("last_seen_at", nil))), want: false},
		{name: "area unknown", in: input(t, uav, with(status())), want: false},
		{name: "battery missing", in: input(t, uav, area(10), with(status("battery_pct", "high"))), want: false},
		{name: "device.status not called", in: input(t, uav, area(10)), want: false},
		{name: "order without device_id fails", in: input(t, uav, noDevice, area(10), with(status())), want: false},
		{name: "status of another device fails", in: input(t, uav, area(10), with(status("device_id", "dev-uav-02"))), want: false},
		{name: "negative battery fails", in: input(t, uav, area(10), with(status("battery_pct", -1.0))), want: false},
	})
	t.Run("threshold carries kb op and need", func(t *testing.T) {
		got := batteryChecker{}.Check(input(t, uav, area(10), with(status("battery_pct", 40.0))))
		if got.Threshold.ThresholdID != thBatteryMargin || got.Threshold.Op != ">=" || got.Threshold.Value != 40 {
			t.Fatalf("threshold %+v", got.Threshold)
		}
	})
}

func TestPayloadChecker(t *testing.T) {
	uav := uavDispatch
	runCases(t, payloadChecker{}, []checkCase{
		{name: "boundary 15 L passes", in: input(t, uav, area(10), with(status("tank_l", 15.0))), want: true, value: 15.0},
		{name: "14.9 L fails", in: input(t, uav, area(10), with(status("tank_l", 14.9))), want: false},
		{name: "empty tank (mock uav)", in: input(t, uav, area(10), with(status("tank_l", 0.0))), want: false},
		{name: "large field capped at 30 L", in: input(t, uav, area(100), with(status("tank_l", 30.0))), want: true},
		{name: "tank missing", in: input(t, uav, area(10), with(status("tank_l", nil))), want: false},
		{name: "negative tank fails", in: input(t, uav, area(10), with(status("tank_l", -2.0))), want: false},
	})
}

func TestRegisteredPrecondition(t *testing.T) {
	notReg := func(in *Input) { in.Product = &Product{CommonName: "x", Ref: "kb:common/pesticide_registry@v1"} }
	runCases(t, registeredPrecondition{}, []checkCase{
		{name: "registered, no tool needed", in: input(t, product(intp(14))), want: true, value: true},
		{name: "not registered", in: input(t, notReg), want: false, value: false},
	})
	if got := (registeredPrecondition{}).Check(input(t)); got.Passed {
		t.Fatalf("no product passed: %+v", got)
	}
}
