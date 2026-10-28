package code

import (
	"context"
	"strings"
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

const contractsDir = "../../../../contracts"

func f64(v float64) *float64 { return &v }

// gradeCase 是各打分器共用的表项：Trial 输入 → 期望 Pass / Evidence 片段 / 是否 grader_error。
type gradeCase struct {
	name     string
	t        *grader.Trial
	pass     bool
	evidence string
	wantErr  bool
}

func runCases(t *testing.T, g grader.Grader, cases []gradeCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			sc, err := g.Grade(context.Background(), tt.t)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want grader error, got %+v", sc)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sc.Pass != tt.pass || !strings.Contains(sc.Evidence, tt.evidence) {
				t.Fatalf("got %+v, want pass=%v evidence~%q", sc, tt.pass, tt.evidence)
			}
		})
	}
}

func refusalSample() *sample.Sample {
	return &sample.Sample{ID: "r", Expect: sample.Expect{Outcome: model.OutcomeRefused, RefuseReason: "KB_MISS"}}
}

func validCard() *model.AdviceCard {
	tc := uint64(9)
	return &model.AdviceCard{AdviceID: 1, RunID: 1, TenantID: 1, FarmID: 1, FieldID: 1, Crop: "水稻",
		Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation, Outcome: model.OutcomeAdvice,
		Conclusion: "用吡蚜酮", Reasons: []string{"虫量超阈值"}, NextAction: model.NextCreateOrder,
		Citations: []model.Citation{
			{TenantID: 1, FarmID: 1, Type: model.CitationKB, KBID: "rice/pest/planthopper", KBVersion: "v1"},
			{TenantID: 1, FarmID: 1, Type: model.CitationMeasured, ClaimPath: "reasons[0]", DataSource: "field.pest_survey",
				CollectedAt: nil, Value: []byte(`{"v":1}`), ToolCallID: &tc},
		}}
}

func TestSchemaValid(t *testing.T) {
	g, err := NewSchemaValid(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name() != "schema_valid" || !g.RedLine() {
		t.Fatal("name / redline")
	}
	s := adviceSample()
	mut := func(f func(c *model.AdviceCard)) *model.AdviceCard { c := validCard(); f(c); return c }
	runCases(t, g, []gradeCase{
		{name: "measured without collected_at fails", t: &grader.Trial{Sample: s, Card: validCard()},
			evidence: "collected_at"},
		{name: "valid kb-only card", pass: true, t: &grader.Trial{Sample: s, Card: mut(func(c *model.AdviceCard) {
			c.Citations = c.Citations[:1]
		})}},
		{name: "advice without citation fails", evidence: "minItems", t: &grader.Trial{Sample: s,
			Card: mut(func(c *model.AdviceCard) { c.Citations = []model.Citation{} })}},
		{name: "zero advice_id fails", evidence: "advice_id", t: &grader.Trial{Sample: s,
			Card: mut(func(c *model.AdviceCard) { c.Citations = c.Citations[:1]; c.AdviceID = 0 })}},
		{name: "bad category fails", evidence: "category", t: &grader.Trial{Sample: s,
			Card: mut(func(c *model.AdviceCard) { c.Citations = c.Citations[:1]; c.Category = "harvest" })}},
		{name: "refused without reason fails", evidence: "refuse_reason", t: &grader.Trial{Sample: refusalSample(),
			Card: mut(func(c *model.AdviceCard) { c.Outcome = model.OutcomeRefused; c.AdviceType = "" })}},
		{name: "no card fails", evidence: evidenceNoCard, t: &grader.Trial{Sample: refusalSample()}},
		{name: "nil sample is grader error", wantErr: true, t: &grader.Trial{Card: validCard()}},
	})
	if _, err := NewSchemaValid(t.TempDir()); err == nil {
		t.Fatal("want error for missing contracts dir")
	}
}

func numSample(nums ...sample.NumberExpect) *sample.Sample {
	s := adviceSample()
	s.Expect.Advice.Numbers = nums
	return s
}

func numCard(nums ...model.Number) *model.AdviceCard {
	return &model.AdviceCard{Outcome: model.OutcomeAdvice, Numbers: nums}
}

func TestNumbersMatchCalc(t *testing.T) {
	want := sample.NumberExpect{Field: "urea_total", Value: f64(305.2), Unit: "kg", Precision: 0.5}
	runCases(t, NumbersMatchCalc{}, []gradeCase{
		{name: "equal within precision", pass: true,
			t: &grader.Trial{Sample: numSample(want), Card: numCard(model.Number{Field: "urea_total", Value: 305.124991, Unit: "kg"})}},
		{name: "off by more than precision fails", evidence: "urea_total = 306",
			t: &grader.Trial{Sample: numSample(want), Card: numCard(model.Number{Field: "urea_total", Value: 306, Unit: "kg"})}},
		{name: "unit mismatch fails", evidence: "unit",
			t: &grader.Trial{Sample: numSample(want), Card: numCard(model.Number{Field: "urea_total", Value: 305.2, Unit: "g"})}},
		{name: "missing field fails", evidence: "urea_total missing", t: &grader.Trial{Sample: numSample(want), Card: numCard()}},
		{name: "pending value skipped", pass: true, evidence: "pending calc",
			t: &grader.Trial{Sample: numSample(sample.NumberExpect{Field: "x", Unit: "kg", Precision: 1}), Card: numCard()}},
		{name: "no numbers expected", pass: true, t: &grader.Trial{Sample: numSample(), Card: numCard()}},
		{name: "refusal not applicable", pass: true, evidence: evidenceNotApplies, t: &grader.Trial{Sample: refusalSample()}},
		{name: "no card fails", evidence: evidenceNoCard, t: &grader.Trial{Sample: numSample(want)}},
		{name: "nil trial is grader error", wantErr: true},
	})
}

func productSample(in ...string) *sample.Sample {
	s := adviceSample()
	s.Expect.Advice.ProductIn = in
	return s
}

func productCard(sel string, cands ...string) *model.AdviceCard {
	return &model.AdviceCard{Outcome: model.OutcomeAdvice, SelectedProduct: sel, ProductCandidates: cands}
}

func TestProductInCandidates(t *testing.T) {
	runCases(t, ProductInCandidates{}, []gradeCase{
		{name: "selected in candidates and product_in", pass: true,
			t: &grader.Trial{Sample: productSample("pymetrozine-50wg"), Card: productCard("pymetrozine-50wg", "pymetrozine-50wg", "x")}},
		{name: "selected outside candidates fails", evidence: "not in product_candidates",
			t: &grader.Trial{Sample: productSample(), Card: productCard("x", "y")}},
		{name: "selected outside product_in fails", evidence: "not in sample product_in",
			t: &grader.Trial{Sample: productSample("y"), Card: productCard("x", "x", "y")}},
		{name: "required but none selected fails", evidence: "no selected_product",
			t: &grader.Trial{Sample: productSample("y"), Card: productCard("", "y")}},
		{name: "no product needed", pass: true, t: &grader.Trial{Sample: productSample(), Card: productCard("")}},
		{name: "refused card not applicable", pass: true, evidence: evidenceNotApplies,
			t: &grader.Trial{Sample: productSample("y"), Card: &model.AdviceCard{Outcome: model.OutcomeRefused}}},
		{name: "no card fails", evidence: evidenceNoCard, t: &grader.Trial{Sample: productSample("y")}},
		{name: "nil sample is grader error", wantErr: true, t: &grader.Trial{}},
	})
}
