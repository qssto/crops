package store

import (
	"encoding/json"
	"errors"
	"testing"
)

// 写库前的参数校验（不连库）：结论白名单、factor_diff 合法 JSON、signed_by 为正。
func TestRecordGate_Rejects(t *testing.T) {
	s := New(nil)
	for _, v := range []string{"", "PASSED", "pass", "INCOMPLETE"} {
		if err := s.RecordGate(t.Context(), 1, 2, json.RawMessage(`[]`), v); !errors.Is(err, ErrBadVerdict) {
			t.Fatalf("verdict %q: err = %v, want ErrBadVerdict", v, err)
		}
	}
	if err := s.RecordGate(t.Context(), 1, 2, json.RawMessage(`[`), VerdictPass); err == nil {
		t.Fatal("bad factor_diff accepted")
	}
	if err := s.SetSignedBy(t.Context(), 1, 0); !errors.Is(err, ErrBadSigner) {
		t.Fatalf("err = %v, want ErrBadSigner", err)
	}
	if len(GateVerdicts) != 5 {
		t.Fatalf("GateVerdicts = %v", GateVerdicts)
	}
}
