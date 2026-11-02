package code

import (
	"strings"
	"testing"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
)

// TestReferenceCheck_Dispatch：expect.dispatch 样本的参考解通过全部打分器；参考解与期望矛盾时应失败。
func TestReferenceCheck_Dispatch(t *testing.T) {
	tx, err := knowledge.LoadTaxonomy(taxonomyFile)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := NewSchemaValid(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	kb := knowledge.NewStore(knowledge.NewRepoReader(repoRoot))
	mk := func(id, final string, returned bool, reapprove string, devices int) *sample.Sample {
		s := withOrder(refAdvice(id, nil), final)
		s.Fixtures.Approver = "approve"
		s.Fixtures.Dispatch = &sample.DispatchFixture{Reapprove: reapprove}
		s.Expect.Dispatch = &sample.DispatchExpect{RecheckReturned: returned, DeviceDispatches: &devices}
		return s
	}
	tests := []struct {
		name      string
		s         *sample.Sample
		violation string
	}{
		{name: "returned stays pending", s: mk("returned", "PENDING_APPROVAL", true, "", 0)},
		{name: "re-dispatch after reopen", s: mk("redispatch", "DONE", true, "approve", 1)},
		{name: "redelivery counts once", s: mk("redeliver", "DONE", false, "", 1)},
		{name: "expired never dispatched", s: mk("expired", "EXPIRED", false, "", 0)},
		// 应失败：退回且未重审却期望设备已下发（与红线④矛盾）
		{name: "returned but expects a dispatch fails", violation: "fails recheck_returned",
			s: mk("contra", "PENDING_APPROVAL", true, "", 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := NewReferenceCheck(tx, kb, All(schema, testActuate)).Check([]*sample.Sample{tt.s})
			var msgs []string
			for _, v := range vs {
				msgs = append(msgs, v.Msg)
			}
			got := strings.Join(msgs, "\n")
			if (tt.violation == "") != (got == "") || !strings.Contains(got, tt.violation) {
				t.Fatalf("violations = %q, want ~%q", got, tt.violation)
			}
		})
	}
}
