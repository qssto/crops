package compare

import (
	"testing"

	"agri-steward/internal/evalkit"
)

// P2 第 3 批：rule_offload.* 与 enable_compaction 是独立因子；快照未记录 flags 按 off，与开关全关的新快照可比。
func TestFlagFactors(t *testing.T) {
	old, off := snap(), snap()
	off.Flags = map[string]string{"enable_compaction": "off"}
	if d := Diff(Factors, old, off); len(d) != 0 {
		t.Fatalf("missing flags vs explicit off must not differ: %+v", d)
	}
	on := snap()
	on.Flags = map[string]string{"rule_offload.tank_mix": "shadow"}
	d, err := Check(Factors, ModeFactor, old, on)
	if err != nil || len(d) != 1 || d[0] != (FactorDiff{"rule_offload.tank_mix", "off", "shadow"}) {
		t.Fatalf("diff = %+v err = %v", d, err)
	}
	// enable_loader_v1 不单列因子：体现为 loader_strategy。
	for _, f := range Factors {
		if f.Name == "enable_loader_v1" {
			t.Fatal("enable_loader_v1 must not be a separate factor")
		}
	}
	bad := snap()
	bad.Flags = map[string]string{"enable_advice": "on"}
	if err := RequireComplete(sideCandidate, bad); err == nil {
		t.Fatal("unsettable flag key must be malformed")
	}
	bad.Flags = map[string]string{"enable_compaction": "shadow"}
	if err := RequireComplete(sideCandidate, bad); err == nil {
		t.Fatal("bool flag with shadow must be malformed")
	}
	if got := len(evalkit.FlagFactors()); got != 4 {
		t.Fatalf("flag factors = %d, want 4", got)
	}
}
