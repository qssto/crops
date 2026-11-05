package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReviewRefKey_MatchesFormula(t *testing.T) {
	exp, idx := uint64(7), 2
	a := ReviewRefKey(ReviewHumanQueue, &exp, "s1", &idx, "fact", "", 0)
	if a != ReviewRefKey(ReviewHumanQueue, &exp, "s1", &idx, "fact", "", RaterPrimary) {
		t.Fatal("rater_no 0 must hash as 1")
	}
	if a == ReviewRefKey(ReviewHumanQueue, &exp, "s1", &idx, "fact", "", RaterSecond) {
		t.Fatal("second rater must have its own key")
	}
	// md5("HUMAN_QUEUE|7|s1|2|fact||1")：与 P2 §6.3 SQL 注释及评测侧既有数据口径一致
	if want := "53076702d70a1a5e8941158c7a3f8502"; a != want {
		t.Fatalf("key = %q, want %q", a, want)
	}
	if ReviewRefKey(ReviewOnlineJudge, nil, "", nil, "", OnlineJudgeRef(1, 2, 3), 0) ==
		ReviewRefKey(ReviewOnlineJudge, nil, "", nil, "", OnlineJudgeRef(1, 2, 4), 0) {
		t.Fatal("different judge rows must not collide")
	}
}

func TestOnlineJudgeRef_RoundTrip(t *testing.T) {
	ref := OnlineJudgeRef(1001, 2001, 42)
	if ref != "ojr:1001:2001:42" {
		t.Fatalf("ref = %q", ref)
	}
	tn, fm, id, err := ParseOnlineJudgeRef(ref)
	if err != nil || tn != 1001 || fm != 2001 || id != 42 {
		t.Fatalf("parse = %d %d %d %v", tn, fm, id, err)
	}
	for _, bad := range []string{"", "ojr:1:2", "ojr:1:2:3:4", "bf-abcdefghijklmnop", "ojr:0:2:3", "ojr:1:x:3", "xyz:1:2:3"} {
		if _, _, _, err := ParseOnlineJudgeRef(bad); err == nil {
			t.Errorf("ParseOnlineJudgeRef(%q) must fail", bad)
		}
	}
}

func TestValidateReviewSubmit(t *testing.T) {
	score := func(n int) *int { return &n }
	tests := []struct {
		name     string
		taskType string
		s        ReviewSubmit
		ok       bool
	}{
		{name: "score 1", taskType: ReviewHumanQueue, s: ReviewSubmit{Score: score(1)}, ok: true},
		{name: "score 3 online", taskType: ReviewOnlineJudge, s: ReviewSubmit{Score: score(3)}, ok: true},
		{name: "calibration 2", taskType: ReviewCalibration, s: ReviewSubmit{Score: score(2)}, ok: true},
		{name: "merge", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionMerge,
			Expect: json.RawMessage(`{"outcome":"ADVICE"}`)}, ok: true},
		{name: "drop", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionDrop, Reason: "重复样本"}, ok: true},
		// 应失败：分数越界、缺分、多带字段、未知类型、MERGE 缺 expect / 非对象、DROP 缺原因
		{name: "score 0", taskType: ReviewHumanQueue, s: ReviewSubmit{Score: score(0)}},
		{name: "score 4", taskType: ReviewMonthlyRecheck, s: ReviewSubmit{Score: score(4)}},
		{name: "score -1", taskType: ReviewOnlineJudge, s: ReviewSubmit{Score: score(-1)}},
		{name: "no score", taskType: ReviewHumanQueue, s: ReviewSubmit{}},
		{name: "score with decision", taskType: ReviewHumanQueue, s: ReviewSubmit{Score: score(2), Decision: "MERGE"}},
		{name: "unknown type", taskType: "OTHER", s: ReviewSubmit{Score: score(2)}},
		{name: "backflow with score", taskType: ReviewBackflowExpect, s: ReviewSubmit{Score: score(2),
			Decision: BackflowDecisionDrop, Reason: "x"}},
		{name: "merge without expect", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionMerge}},
		{name: "merge array expect", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionMerge,
			Expect: json.RawMessage(`[1]`)}},
		{name: "merge empty object", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionMerge,
			Expect: json.RawMessage(`{}`)}},
		{name: "merge null expect", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionMerge,
			Expect: json.RawMessage(`null`)}},
		{name: "drop blank reason", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionDrop, Reason: "  "}},
		{name: "drop with expect", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: BackflowDecisionDrop, Reason: "x",
			Expect: json.RawMessage(`{"a":1}`)}},
		{name: "unknown decision", taskType: ReviewBackflowExpect, s: ReviewSubmit{Decision: "KEEP"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateReviewSubmit(tt.taskType, tt.s)
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrReviewSubmit) {
				t.Fatalf("err = %v, want ErrReviewSubmit", err)
			}
		})
	}
}

// 红线（P2-L2b）：holdout 任务的视图不带任何样本定位（样本 ID / trial / 实验 / 草稿引用 / judge 版本），
// 也不回 judge 分（盲评）；他人的专家分不回。
func TestReviewTaskView_HoldoutHasNoMaterial(t *testing.T) {
	exp, idx, judge, expert, me, other := uint64(7), 2, 1, 3, uint64(9002), uint64(9003)
	base := ReviewTask{ID: 5, TaskType: ReviewHumanQueue, QueueReason: "UNKNOWN", ExperimentID: &exp, SampleID: "secret-sample-id",
		TrialIndex: &idx, DraftRef: "bf-x", Dimension: "fact", JudgeVersion: "j-secret", JudgeScore: &judge, RaterNo: 1,
		ExpertScore: &expert, AssigneeID: &me, Status: ReviewDone}
	for _, tt := range []struct {
		name    string
		holdout bool
		viewer  uint64
		absent  []string
		present []string
	}{
		{name: "holdout", holdout: true, viewer: me,
			absent:  []string{"secret-sample-id", "experiment_id", "trial_index", "j-secret", "bf-x", "material", "judge_score", "UNKNOWN"},
			present: []string{`"holdout":true`, `"expert_score":3`, `"mine":true`}},
		{name: "regular other viewer", viewer: other,
			absent:  []string{"judge_score", "expert_score"},
			present: []string{"secret-sample-id", `"mine":false`, `"material"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			task := base
			task.Holdout = tt.holdout
			b, err := json.Marshal(task.View(tt.viewer))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range tt.absent {
				if strings.Contains(string(b), s) {
					t.Errorf("view leaks %q: %s", s, b)
				}
			}
			for _, s := range tt.present {
				if !strings.Contains(string(b), s) {
					t.Errorf("view missing %q: %s", s, b)
				}
			}
		})
	}
}

func TestReviewTaskView_OnlineJudgeTarget(t *testing.T) {
	v := ReviewTask{ID: 1, TaskType: ReviewOnlineJudge, DraftRef: OnlineJudgeRef(1, 2, 3)}.View(9002)
	if v.Material == nil || v.Material.OnlineJudge == nil || *v.Material.OnlineJudge != (OnlineJudgeTarget{1, 2, 3}) {
		t.Fatalf("view = %+v", v.Material)
	}
	if bad := (ReviewTask{ID: 1, TaskType: ReviewOnlineJudge, DraftRef: "ojr:x"}).View(9002); bad.Material.OnlineJudge != nil {
		t.Fatal("bad ref must not produce a target")
	}
}
