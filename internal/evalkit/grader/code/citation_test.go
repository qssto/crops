package code

import (
	"context"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

type kbSet map[string]bool

func (k kbSet) Has(id, v string) bool { return k[id+"@"+v] }

var testKB = kbSet{"rice/pest/planthopper@v1": true, "common/safety/phi@v1": true}

func kbCite(path, id, v string) model.Citation {
	return model.Citation{Type: model.CitationKB, ClaimPath: path, KBID: id, KBVersion: v}
}

func adviceSample(mustCite ...string) *sample.Sample {
	return &sample.Sample{ID: "s1", Expect: sample.Expect{Outcome: model.OutcomeAdvice,
		Advice: &sample.AdviceExpect{MustCite: mustCite}}}
}

func card(cites ...model.Citation) *model.AdviceCard {
	return &model.AdviceCard{Outcome: model.OutcomeAdvice, Conclusion: "用吡蚜酮防治",
		Reasons: []string{"百丛虫量超阈值"}, Numbers: []model.Number{{Field: "total_dose", Value: 250, Unit: "g"}},
		Citations: cites}
}

func TestCitationValid(t *testing.T) {
	full := []model.Citation{
		kbCite("", "rice/pest/planthopper", "v1"),
		{Type: model.CitationMeasured, ClaimPath: "reasons[0]", DataSource: "pest.trap"},
		kbCite("numbers[0]", "common/safety/phi", "v1"),
	}
	tests := []struct {
		name     string
		s        *sample.Sample
		c        *model.AdviceCard
		kb       grader.KnowledgeIndex
		pass     bool
		evidence string // 失败时 Evidence 须包含
		value    float64
		wantErr  bool
	}{
		{name: "all fields cited and must_cite present", s: adviceSample("kb:rice/pest/planthopper@v1"),
			c: card(full...), kb: testKB, pass: true, value: 1},
		{name: "reason without citation fails", s: adviceSample(),
			c: card(full[0], full[2]), kb: testKB, evidence: "reasons[0] has no citation", value: 2.0 / 3},
		{name: "number without citation fails", s: adviceSample(),
			c: card(full[0], full[1]), kb: testKB, evidence: "numbers[0] has no citation", value: 2.0 / 3},
		{name: "kb version not in knowledge version fails", s: adviceSample(),
			c: card(kbCite("", "rice/pest/planthopper", "v9"), full[1], full[2]), kb: testKB,
			evidence: "kb:rice/pest/planthopper@v9 not in knowledge version", value: 1},
		{name: "unknown kb_id fails", s: adviceSample(),
			c: card(kbCite("", "rice/pest/locust", "v1"), full[1], full[2]), kb: testKB,
			evidence: "rice/pest/locust@v1 not in knowledge version", value: 1},
		{name: "must_cite missing fails", s: adviceSample("kb:common/safety/phi@v1", "kb:rice/pest/planthopper@v1"),
			c: card(full[0], full[1], kbCite("numbers[0]", "rice/pest/planthopper", "v1")), kb: testKB,
			evidence: "must_cite kb:common/safety/phi@v1 not cited", value: 1},
		{name: "must_cite version mismatch fails", s: adviceSample("kb:rice/pest/planthopper@v2"),
			c: card(full...), kb: testKB, evidence: "must_cite kb:rice/pest/planthopper@v2 not cited", value: 1},
		{name: "dangling claim_path fails", s: adviceSample(),
			c: card(append(full, kbCite("reasons[3]", "rice/pest/planthopper", "v1"))...), kb: testKB,
			evidence: `claim_path "reasons[3]" points to no field`, value: 1},
		{name: "measured citation does not satisfy must_cite", s: adviceSample("kb:rice/pest/planthopper@v1"),
			c: card(model.Citation{Type: model.CitationMeasured, ClaimPath: "conclusion"}, full[1], full[2]), kb: testKB,
			evidence: "must_cite kb:rice/pest/planthopper@v1 not cited", value: 1},
		{name: "no card when advice expected fails", s: adviceSample(), kb: testKB, evidence: "no advice card"},
		{name: "refusal sample not applicable", kb: testKB, pass: true, value: 1,
			s: &sample.Sample{ID: "r", Expect: sample.Expect{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}}},
		{name: "refused card not applicable", s: adviceSample(), kb: testKB, pass: true, value: 1,
			c: &model.AdviceCard{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}},
		{name: "malformed must_cite is grader error", s: adviceSample("rice/pest/planthopper@v1"),
			c: card(full...), kb: testKB, wantErr: true},
		{name: "nil knowledge version is grader error", s: adviceSample(), c: card(full...), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := CitationValid{}.Grade(context.Background(), &grader.Trial{Sample: tt.s, Card: tt.c, KB: tt.kb})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want grader error, got %+v", sc)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sc.Pass != tt.pass || !strings.Contains(sc.Evidence, tt.evidence) || sc.Value != tt.value {
				t.Fatalf("got %+v, want pass=%v value=%v evidence~%q", sc, tt.pass, tt.value, tt.evidence)
			}
		})
	}
	if g := (CitationValid{}); g.Name() != "citation_valid" || !g.RedLine() {
		t.Fatal("name / redline")
	}
}
