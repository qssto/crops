package contracts_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agri-steward/contracts"
	"agri-steward/internal/model"
)

// scoreTaskTypes 是只带 score 的任务类型（契约不区分任务类型，任一类型判合法即算合法）。
var scoreTaskTypes = []string{model.ReviewCalibration, model.ReviewHumanQueue, model.ReviewMonthlyRecheck, model.ReviewOnlineJudge}

func submitValidForSomeType(s model.ReviewSubmit) bool {
	for _, tt := range append(scoreTaskTypes, model.ReviewBackflowExpect) {
		if model.ValidateReviewSubmit(tt, s) == nil {
			return true
		}
	}
	return false
}

func toDoc(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// P2-L2b：提交体契约与 model.ValidateReviewSubmit 判定一致（契约漂移即失败）。expect 的 16KiB 字节上限只在服务端校验。
func TestReviewSubmitRequestContract(t *testing.T) {
	sch := compileContract(t, contracts.ReviewSubmitRequestFile)
	n := func(v int) *int { return &v }
	obj := json.RawMessage(`{"outcome":"ADVICE"}`)
	cases := []model.ReviewSubmit{
		{Score: n(1)}, {Score: n(3)}, {Score: n(0)}, {Score: n(4)}, {},
		{Score: n(2), Reason: "x"}, {Score: n(2), Decision: model.BackflowDecisionMerge},
		{Decision: model.BackflowDecisionMerge, Expect: obj},
		{Decision: model.BackflowDecisionMerge, Expect: obj, Reason: "补齐"},
		{Decision: model.BackflowDecisionMerge},
		{Decision: model.BackflowDecisionMerge, Expect: json.RawMessage(`{}`)},
		{Decision: model.BackflowDecisionMerge, Expect: json.RawMessage(`["a"]`)},
		{Decision: model.BackflowDecisionMerge, Expect: obj, Score: n(2)},
		{Decision: model.BackflowDecisionDrop, Reason: "重复"},
		{Decision: model.BackflowDecisionDrop, Reason: "  "},
		{Decision: model.BackflowDecisionDrop},
		{Decision: model.BackflowDecisionDrop, Reason: "重复", Expect: obj},
		{Decision: model.BackflowDecisionDrop, Reason: strings.Repeat("字", model.MaxReviewReason+1)},
		{Decision: "KEEP", Reason: "x"},
	}
	for _, c := range cases {
		schemaOK := sch.Validate(toDoc(t, c)) == nil
		if modelOK := submitValidForSomeType(c); schemaOK != modelOK {
			t.Errorf("%+v (expect %s): schema ok=%v, model ok=%v", c, c.Expect, schemaOK, modelOK)
		}
	}
	if sch.Validate(map[string]any{"score": 2, "judge_score": 3}) == nil {
		t.Error("unknown field must be rejected")
	}
}

// 红线：model.ReviewTask.View 的输出符合契约；holdout 视图不带 material，契约也拒绝带 material 的 holdout 视图。
func TestReviewTaskViewContract(t *testing.T) {
	sch := compileContract(t, contracts.ReviewTaskViewFile)
	exp, idx, expert, score := uint64(7), 0, uint64(9002), 2
	at := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	base := model.ReviewTask{ID: 1, TaskType: model.ReviewHumanQueue, QueueReason: "DISAGREE", ExperimentID: &exp,
		SampleID: "s-1", TrialIndex: &idx, Dimension: "fact", JudgeVersion: "j1", JudgeScore: &score, RaterNo: 1,
		AssigneeID: &expert, ExpertScore: &score, Status: model.ReviewDone, CreatedAt: at, DoneAt: &at}
	hold := base
	hold.Holdout = true
	oj := model.ReviewTask{ID: 2, TaskType: model.ReviewOnlineJudge, DraftRef: model.OnlineJudgeRef(1, 2, 3), RaterNo: 1,
		Status: model.ReviewOpen, CreatedAt: at}
	withTrial := base.View(expert)
	withTrial.Material.Trial = &model.ReviewTrial{Suite: "regression", SampleVersion: 2, Status: "PASS"}
	for name, v := range map[string]model.ReviewTaskView{"regular": base.View(expert), "other expert": base.View(9003),
		"holdout": hold.View(expert), "online judge": oj.View(expert), "with trial": withTrial} {
		if err := sch.Validate(toDoc(t, v)); err != nil {
			t.Errorf("%s view violates contract: %v", name, err)
		}
	}
	leaked := hold.View(expert)
	leaked.Material = &model.ReviewMaterial{SampleID: "s-1"}
	if sch.Validate(toDoc(t, leaked)) == nil {
		t.Error("holdout view with material must violate the contract")
	}
	doc := toDoc(t, base.View(expert)).(map[string]any)
	doc["judge_score"] = 2
	if sch.Validate(doc) == nil {
		t.Error("judge_score must not be in the view (blind review)")
	}
}
