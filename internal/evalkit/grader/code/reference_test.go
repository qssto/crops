package code

import (
	"strings"
	"testing"

	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const (
	repoRoot     = "../../../.."
	taxonomyFile = repoRoot + "/eval/taxonomy.yaml"
)

func refAdvice(id string, mut func(a *sample.AdviceExpect, tr *sample.Trajectory)) *sample.Sample {
	s := &sample.Sample{ID: id, Tags: []string{"L4", "protect.timing"}, KnowledgeRef: knowledge.RefWorktree,
		Expect: sample.Expect{Outcome: model.OutcomeAdvice, Advice: &sample.AdviceExpect{
			Category: "protect", AdviceType: "作业类", Conclusion: "建议施药",
			MustCite: []string{"kb:rice/pest/planthopper@v1"}}}}
	if mut != nil {
		mut(s.Expect.Advice, &s.Expect.Trajectory)
	}
	return s
}

func TestReferenceCheck(t *testing.T) {
	tx, err := knowledge.LoadTaxonomy(taxonomyFile)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := NewSchemaValid(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	kb := knowledge.NewStore(knowledge.NewRepoReader(repoRoot))
	refusal := &sample.Sample{ID: "ref-refusal", Tags: []string{"L4", "protect.timing"}, KnowledgeRef: knowledge.RefWorktree,
		Expect: sample.Expect{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}}
	tests := []struct {
		name      string
		s         *sample.Sample
		violation string // 空表示应通过
		note      string // Notes 须包含
	}{
		{name: "advice with numbers and must_call passes", s: refAdvice("ok", func(a *sample.AdviceExpect, tr *sample.Trajectory) {
			a.Numbers = []sample.NumberExpect{{Field: "dose", Value: f64(25), Unit: "mL", Precision: 0.1}}
			tr.MustCall = []string{"crop.phenology"}
			tr.MustNotCall = []string{"device.*"}
			tr.Budget = &sample.Budget{MaxSteps: 1, MaxTokens: 4000}
		}), note: skipMaxTokens},
		{name: "refusal passes", s: refusal},
		{name: "pending number is noted not failed", s: refAdvice("pending", func(a *sample.AdviceExpect, _ *sample.Trajectory) {
			a.Numbers = []sample.NumberExpect{{Field: "dose", Unit: "mL", Precision: 0.1}}
		}), note: skipNumberPending},
		{name: "unknown kb version fails citation_valid", violation: "fails citation_valid",
			s: refAdvice("bad-kb", func(a *sample.AdviceExpect, _ *sample.Trajectory) {
				a.MustCite = []string{"kb:rice/pest/planthopper@v9"}
			})},
		{name: "bad category fails schema_valid", violation: "fails schema_valid",
			s: refAdvice("bad-cat", func(a *sample.AdviceExpect, _ *sample.Trajectory) { a.Category = "harvest" })},
		{name: "must_call over budget fails", violation: "fails budget",
			s: refAdvice("over", func(_ *sample.AdviceExpect, tr *sample.Trajectory) {
				tr.MustCall = []string{"a.b", "c.d"}
				tr.Budget = &sample.Budget{MaxSteps: 1}
			})},
		{name: "must_call contradicts must_not_call fails", violation: "fails must_call",
			s: refAdvice("contra", func(_ *sample.AdviceExpect, tr *sample.Trajectory) {
				tr.MustCall = []string{"device.list"}
				tr.MustNotCall = []string{"device.*"}
			})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := NewReferenceCheck(tx, kb, All(schema))
			vs := rc.Check([]*sample.Sample{tt.s})
			var msgs []string
			for _, v := range vs {
				if v.Rule != RuleReferencePasses || v.SampleID != tt.s.ID {
					t.Errorf("violation %+v", v)
				}
				msgs = append(msgs, v.Msg)
			}
			got := strings.Join(msgs, "\n")
			if (tt.violation == "") != (got == "") || !strings.Contains(got, tt.violation) {
				t.Fatalf("violations = %q, want ~%q", got, tt.violation)
			}
			if notes := strings.Join(rc.Notes(), "\n"); !strings.Contains(notes, tt.note) {
				t.Fatalf("notes = %q, want ~%q", notes, tt.note)
			}
		})
	}
}
