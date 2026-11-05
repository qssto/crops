package sample

import (
	"strings"
	"testing"
)

// TestCheckConflict（P2 L4）：expect.conflict 的类别须已知，tank_mix 至少两种药剂、都在问题里、按混用禁忌表构成禁忌。
func TestCheckConflict(t *testing.T) {
	const q = "氰氟草酯和二甲四氯能一桶混着打吗？"
	tests := []struct {
		name    string
		c       *ConflictExpect
		wantMsg string // 空表示应通过
	}{
		{name: "not declared"},
		{name: "forbidden pair", c: &ConflictExpect{Kind: ConflictKindTankMix, Products: []string{"氰氟草酯", "二甲四氯"}}},
		{name: "unknown kind", c: &ConflictExpect{Kind: "interval"}, wantMsg: "is not one of"},
		{name: "single product", c: &ConflictExpect{Kind: ConflictKindTankMix, Products: []string{"氰氟草酯"}},
			wantMsg: "at least two products"},
		{name: "product not in question", c: &ConflictExpect{Kind: ConflictKindTankMix, Products: []string{"氰氟草酯", "麦草畏"}},
			wantMsg: "does not appear in question"},
		{name: "pair not forbidden", c: &ConflictExpect{Kind: ConflictKindTankMix, Products: []string{"氰氟草酯", "吡蚜酮"}},
			wantMsg: "form no forbidden tank mix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sample{Question: q + "吡蚜酮", Expect: Expect{Conflict: tt.c}}
			msgs := checkConflict(s)
			if tt.wantMsg == "" {
				if len(msgs) != 0 {
					t.Fatalf("msgs = %v, want none", msgs)
				}
				return
			}
			if !strings.Contains(strings.Join(msgs, "; "), tt.wantMsg) {
				t.Fatalf("msgs = %v, want ~%q", msgs, tt.wantMsg)
			}
		})
	}
}
