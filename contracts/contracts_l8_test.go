package contracts_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"agri-steward/contracts"
	"agri-steward/internal/model"
)

func compileContract(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	b, err := contracts.FS.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	if err := comp.AddResource(file, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := comp.Compile(file)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

// P2 L8：反馈请求契约与 model.ValidateFeedback 判定一致（契约漂移即失败）。
func TestAdviceFeedbackRequestContract(t *testing.T) {
	sch := compileContract(t, contracts.AdviceFeedbackRequestFile)
	cases := []struct {
		verdict  model.FeedbackVerdict
		category model.RejectReasonCode
		comment  string
		reqID    string
	}{
		{model.FeedbackAdopt, "", "", "r1"},
		{model.FeedbackAdopt, "", "好", "r1"},
		{model.FeedbackAdopt, model.RejectDosage, "", "r1"},
		{model.FeedbackDislike, model.RejectTiming, "", "r1"},
		{model.FeedbackDislike, "", "", "r1"},
		{model.FeedbackDislike, "WEATHER", "", "r1"},
		{model.FeedbackDislike, model.RejectOther, "", "r1"},
		{model.FeedbackDislike, model.RejectOther, "  ", "r1"},
		{model.FeedbackDislike, model.RejectOther, "太贵", "r1"},
		{"LIKE", "", "", "r1"},
		{model.FeedbackAdopt, "", "", ""},
		{model.FeedbackAdopt, "", "", strings.Repeat("a", model.MaxFeedbackRequestID+1)},
		{model.FeedbackAdopt, "", strings.Repeat("字", model.MaxRejectReasonText+1), "r1"},
	}
	for _, c := range cases {
		doc := map[string]any{"verdict": string(c.verdict), "request_id": c.reqID}
		if c.category != "" {
			doc["category"] = string(c.category)
		}
		if c.comment != "" {
			doc["comment"] = c.comment
		}
		schemaOK := sch.Validate(doc) == nil
		modelOK := model.ValidateFeedback(c.verdict, c.category, c.comment, c.reqID) == nil
		if schemaOK != modelOK {
			t.Errorf("%+v: schema ok=%v, model ok=%v", c, schemaOK, modelOK)
		}
	}
}
