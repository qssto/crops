package guardrail

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/agent/calc"
	"agri-steward/internal/model"
)

type fakeKB struct {
	have       map[string]bool // kb_id@version
	deprecated map[string]bool
}

func (k fakeKB) Has(id, ver string) bool   { return k.have[id+"@"+ver] }
func (k fakeKB) Deprecated(id string) bool { return k.deprecated[id] }
func (k fakeKB) Ref() string               { return "kb@test" }
func ptr[T any](v T) *T                    { return &v }
func kbCite(path string) model.Citation {
	return model.Citation{TenantID: 1, FarmID: 1, Type: model.CitationKB, ClaimPath: path, KBID: "rice/pest/planthopper", KBVersion: "v1"}
}

var t0 = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)

// validInput 是一张通过全部校验的施药建议卡。
func validInput() Input {
	measured := model.Citation{TenantID: 1, FarmID: 1, Type: model.CitationMeasured, ClaimPath: "reasons[0]",
		DataSource: "mcp-weather", CollectedAt: &t0, Value: json.RawMessage(`2.1`), ToolCallID: ptr(uint64(7))}
	card := &model.AdviceCard{
		RunID: 2, TenantID: 1, FarmID: 1, FieldID: 3, Crop: "rice",
		Category: model.CategoryProtect, AdviceType: model.AdviceTypeOperation, Outcome: model.OutcomeAdvice,
		Conclusion: "建议施用吡蚜酮 20 g/亩", Reasons: []string{"虫口密度达标"},
		ProductCandidates: []string{"吡蚜酮", "噻虫嗪"}, SelectedProduct: "吡蚜酮",
		Numbers:    []model.Number{{Field: "dose_per_mu", Value: 20, Unit: "g/亩"}},
		Citations:  []model.Citation{kbCite(""), measured, kbCite("numbers[0]")},
		Confidence: 0.8, NextAction: model.NextCreateOrder,
	}
	return Input{
		Card: card,
		Numbers: []Number{{Field: "dose_per_mu", Result: calc.Result{Value: 20.04, Unit: "g/亩", Precision: 0.1,
			Trace: calc.Trace{Formula: "rice/pest/planthopper@v1#dose"}}}},
		Candidates:     []string{"吡蚜酮", "噻虫嗪"},
		ModelText:      []string{"建议施用吡蚜酮 {{n0}}", "虫口密度达标"},
		RequireProduct: true,
		KB:             fakeKB{have: map[string]bool{"rice/pest/planthopper@v1": true}},
	}
}

func TestValidate(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Input)
		want   Check // "" 表示通过
		detail string
	}{
		{name: "valid", mutate: func(*Input) {}},
		{name: "nil card", mutate: func(in *Input) { in.Card = nil }, want: CheckSchema},
		{name: "schema: conclusion too long", mutate: func(in *Input) { in.Card.Conclusion = strings.Repeat("长", 513) }, want: CheckSchema},
		{name: "schema: bad outcome", mutate: func(in *Input) { in.Card.Outcome = "MAYBE" }, want: CheckSchema},
		{name: "schema: advice without advice_type", mutate: func(in *Input) { in.Card.AdviceType = "" }, want: CheckSchema},
		{name: "schema: advice without citation", mutate: func(in *Input) { in.Card.Citations = nil }, want: CheckSchema},
		{name: "refused card skips content checks", mutate: func(in *Input) {
			in.Card.Outcome, in.Card.RefuseReason = model.OutcomeRefused, "KB_MISS"
			in.Card.Numbers, in.Numbers = nil, []Number{{Field: "x"}}
			in.Card.SelectedProduct = ""
		}},
		{name: "citation: reason uncovered", mutate: func(in *Input) { in.Card.Citations = in.Card.Citations[:1] },
			want: CheckCitation, detail: "reasons[0] has no citation"},
		{name: "citation: number uncovered", mutate: func(in *Input) { in.Card.Citations = in.Card.Citations[:2] },
			want: CheckCitation, detail: "numbers[0] has no citation"},
		{name: "citation: dangling claim_path", mutate: func(in *Input) {
			in.Card.Citations = append(in.Card.Citations, kbCite("reasons[5]"))
		}, want: CheckCitation, detail: "points to no field"},
		{name: "citation: unknown kb version", mutate: func(in *Input) { in.Card.Citations[0].KBVersion = "v9" }, want: CheckCitation},
		{name: "citation: deprecated kb", mutate: func(in *Input) {
			in.KB = fakeKB{have: map[string]bool{"rice/pest/planthopper@v1": true}, deprecated: map[string]bool{"rice/pest/planthopper": true}}
		}, want: CheckCitation},
		// 契约 citation schema 已要求 measured 带 tool_call_id，故先被 schema 拦下。
		{name: "measured without tool_call_id", mutate: func(in *Input) { in.Card.Citations[1].ToolCallID = nil },
			want: CheckSchema, detail: "tool_call_id"},
		{name: "citation: no knowledge snapshot", mutate: func(in *Input) { in.KB = nil }, want: CheckCitation},
		{name: "number: tampered value", mutate: func(in *Input) { in.Card.Numbers[0].Value = 30 },
			want: CheckNumber, detail: "calc says"},
		{name: "number: tampered unit", mutate: func(in *Input) { in.Card.Numbers[0].Unit = "mL/亩" }, want: CheckNumber},
		{name: "number: extra card number", mutate: func(in *Input) {
			in.Card.Numbers = append(in.Card.Numbers, model.Number{Field: "dose_total", Value: 1, Unit: "g"})
			in.Card.Citations = append(in.Card.Citations, kbCite("numbers[1]"))
		}, want: CheckNumber, detail: "card has 2 numbers"},
		{name: "number: no calc trace", mutate: func(in *Input) { in.Numbers[0].Result.Trace.Formula = "" },
			want: CheckNumber, detail: "no calc trace"},
		{name: "number: non-finite", mutate: func(in *Input) { in.Numbers[0].Result.Value = math.NaN() }, want: CheckNumber},
		{name: "number: bare digit in model text", mutate: func(in *Input) { in.ModelText[1] = "虫口密度 25 头/百丛" },
			want: CheckNumber, detail: "bare number 25"},
		{name: "number: full-width digit", mutate: func(in *Input) { in.ModelText[1] = "密度２５头" }, want: CheckNumber},
		{name: "number: placeholder out of range", mutate: func(in *Input) { in.ModelText[0] = "用量 {{n3}}" },
			want: CheckNumber, detail: "{{n3}}"},
		{name: "product: selected out of set", mutate: func(in *Input) {
			in.Card.SelectedProduct = "敌敌畏"
			in.Card.ProductCandidates = append(in.Card.ProductCandidates, "敌敌畏")
		}, want: CheckProduct, detail: "candidate 敌敌畏"},
		{name: "product: selected not in rule candidates", mutate: func(in *Input) { in.Card.SelectedProduct = "敌敌畏" },
			want: CheckProduct, detail: "selected product"},
		{name: "product: spray without product", mutate: func(in *Input) { in.Card.SelectedProduct = "" },
			want: CheckProduct, detail: "without selected product"},
		{name: "product: not required", mutate: func(in *Input) { in.Card.SelectedProduct, in.RequireProduct = "", false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.mutate(&in)
			got := v.Validate(in)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("want pass, got %v", got)
				}
				return
			}
			if got == nil || got.Check != tt.want || !strings.Contains(got.Detail, tt.detail) {
				t.Fatalf("got %v, want %s containing %q", got, tt.want, tt.detail)
			}
		})
	}
}

// TestValidate_OrderSchemaFirst：同时违反多项时报告最先的校验项。
func TestValidate_OrderSchemaFirst(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.Card.Outcome = "MAYBE"
	in.Card.Numbers[0].Value = 99
	in.Card.SelectedProduct = "敌敌畏"
	if got := v.Validate(in); got == nil || got.Check != CheckSchema {
		t.Fatalf("got %v, want schema", got)
	}
	in = validInput()
	in.Card.Numbers[0].Value = 99
	in.Card.SelectedProduct = "敌敌畏"
	if got := v.Validate(in); got == nil || got.Check != CheckNumber {
		t.Fatalf("got %v, want number", got)
	}
}

func TestValidate_DoesNotMutateCard(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := validInput()
	if p := v.Validate(in); p != nil {
		t.Fatal(p)
	}
	if in.Card.AdviceID != 0 {
		t.Fatalf("advice_id mutated to %d", in.Card.AdviceID)
	}
}
