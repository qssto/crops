package report

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
)

// holdoutExperiment 在 testExperiment 上追加两个 holdout trial：一个 FAIL（证据含样本内容）、一个 INFRA_ERROR。
func holdoutExperiment() *evalkit.Experiment {
	exp := testExperiment()
	exp.Suites = []string{"regression", "holdout"}
	card := &model.AdviceCard{}
	exp.Trials = append(exp.Trials,
		evalkit.Trial{SampleID: "rice-ho-prot-9901", Suite: "holdout", Tags: []string{"protect.secret_topic"}, TrialIndex: 0,
			Expected: model.OutcomeAdvice, Outcome: model.OutcomeAdvice, Card: card, RefuseReason: "SECRET_REASON",
			UnexpectedCalls: []string{"secret.tool"},
			Scores:          []evalkit.Score{{Name: "citation_valid", RedLine: true, Evidence: "SECRET_EVIDENCE 稻飞虱 20ml"}}},
		evalkit.Trial{SampleID: "rice-ho-prot-9902", Suite: "holdout", TrialIndex: 0, Expected: model.OutcomeAdvice,
			Error: "SECRET_ERROR question text"},
	)
	return exp
}

func TestTranscriptKey_Holdout(t *testing.T) {
	pub := evalkit.Trial{SampleID: "s-1", Suite: "regression", TrialIndex: 2}
	ho := evalkit.Trial{SampleID: "rice-ho-prot-9901", Suite: "holdout", TrialIndex: 2}
	if got := TranscriptKey("7", pub); got != "transcript/7/s-1/2.jsonl" {
		t.Errorf("public key = %s", got)
	}
	if got := TranscriptKey("7", ho); got != "report/holdout/transcript/7/rice-ho-prot-9901/2.jsonl" {
		t.Errorf("holdout key = %s, want under %s/", got, HoldoutPrefix)
	}
}

// 公开报告里 holdout 只给 ID 与打分器名，不给证据、错误文本、tag、卡片（P2 §6.3 任务 9）。
func TestRedactHoldout_PublicReport(t *testing.T) {
	exp := holdoutExperiment()
	red := RedactHoldout(exp)
	var b strings.Builder
	if err := Render(&b, red); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, secret := range []string{"SECRET_EVIDENCE", "SECRET_ERROR", "SECRET_REASON", "secret_topic", "secret.tool"} {
		if strings.Contains(out, secret) {
			t.Errorf("public report leaks %q", secret)
		}
	}
	for _, want := range []string{"rice-ho-prot-9901", "rice-ho-prot-9902", "citation_valid"} {
		if !strings.Contains(out, want) {
			t.Errorf("public report missing %q", want)
		}
	}
	// 状态不变：脱敏不改通过 / 失败 / 出错的判定。
	for i := range exp.Trials {
		if exp.Trials[i].Status() != red.Trials[i].Status() {
			t.Errorf("trial %d status %s -> %s", i, exp.Trials[i].Status(), red.Trials[i].Status())
		}
	}
	// 原实验不被修改；公开样本原样保留。
	if exp.Trials[4].Scores[0].Evidence == RedactedText || exp.Trials[5].Error == RedactedText {
		t.Fatal("RedactHoldout mutated the input")
	}
	if !slices.Equal(red.Trials[0].Tags, exp.Trials[0].Tags) || red.Trials[2].Error != exp.Trials[2].Error {
		t.Fatal("public trials changed")
	}
}

// L2 评审 P1-2：holdout 按加载时的路径标判定，suite 字段写成公开套件也照样脱敏、进 holdout 前缀。
func TestIsHoldoutTrial_ByPathTag(t *testing.T) {
	tagged := evalkit.Trial{SampleID: "rice-ho-prot-9903", Suite: "regression", Holdout: true, TrialIndex: 0,
		Scores: []evalkit.Score{{Name: "citation_valid", Evidence: "SECRET_EVIDENCE"}}}
	if !IsHoldoutTrial(tagged) {
		t.Fatal("path-tagged trial not treated as holdout")
	}
	if got := TranscriptKey("7", tagged); !strings.HasPrefix(got, HoldoutPrefix+"/") {
		t.Fatalf("transcript key = %s", got)
	}
	exp := testExperiment()
	exp.Trials = append(exp.Trials, tagged)
	red := RedactHoldout(exp)
	if ev := red.Trials[len(red.Trials)-1].Scores[0].Evidence; strings.Contains(ev, "SECRET") {
		t.Fatalf("tagged trial evidence not redacted: %q", ev)
	}
}

// L2 评审 P2-7：holdout 隔离样本在公开报告里只留 ID，隔离理由（可能含题面）替换为占位；公开隔离样本原样保留，不修改输入。
func TestRedactHoldout_Quarantined(t *testing.T) {
	exp := testExperiment()
	exp.Quarantined = []evalkit.QuarantinedSample{
		{ID: "rice-pub-0009", Suite: "regression", Reason: "PUBLIC_REASON"},
		{ID: "rice-ho-prot-9909", Suite: "holdout", Reason: "SECRET_QUARANTINE 稻飞虱题面"},
		{ID: "rice-ho-prot-9910", Suite: "regression", Holdout: true, Reason: "SECRET_TAGGED"},
	}
	red := RedactHoldout(exp)
	var b strings.Builder
	if err := Render(&b, red); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, secret := range []string{"SECRET_QUARANTINE", "SECRET_TAGGED"} {
		if strings.Contains(out, secret) {
			t.Errorf("public report leaks quarantine reason %q", secret)
		}
	}
	for _, want := range []string{"rice-ho-prot-9909", "rice-ho-prot-9910", "rice-pub-0009", "PUBLIC_REASON"} {
		if !strings.Contains(out, want) {
			t.Errorf("public report missing %q", want)
		}
	}
	for _, q := range red.Quarantined[1:] {
		if q.Reason != RedactedText || q.Suite != "holdout" || !q.Holdout {
			t.Errorf("redacted quarantined = %+v, want ID only (suite holdout, reason %q)", q, RedactedText)
		}
	}
	if exp.Quarantined[1].Reason == RedactedText || exp.Quarantined[2].Suite != "regression" {
		t.Fatal("RedactHoldout mutated the input")
	}
}

func TestUploadHoldoutReport(t *testing.T) {
	const bucket = "agri-steward-eval"
	obj := newMemPutter()
	key, err := UploadHoldoutReport(context.Background(), obj, bucket, testExperiment())
	if err != nil || key != "" || len(obj.objects) != 0 {
		t.Fatalf("no holdout trial: key=%q err=%v objects=%d", key, err, len(obj.objects))
	}
	exp := holdoutExperiment()
	key, err = UploadHoldoutReport(context.Background(), obj, bucket, exp)
	if err != nil {
		t.Fatal(err)
	}
	if key != "report/holdout/"+exp.ID+"/report.md" {
		t.Fatalf("key = %s", key)
	}
	body := string(obj.objects[bucket+"/"+key])
	// 负责人可读的明细保留证据；只含 holdout trial。
	if !strings.Contains(body, "SECRET_EVIDENCE") || strings.Contains(body, "| s-1 |") {
		t.Fatalf("holdout report body:\n%s", body)
	}
	obj.failKey = key
	if _, err := UploadHoldoutReport(context.Background(), obj, bucket, exp); err == nil {
		t.Fatal("upload failure swallowed")
	}
}

// L2 评审裁决：报告分项打分写 judge 出错数（门禁未开，不改状态）；holdout trial 的 AdvisoryError 文本脱敏。
func TestRender_AdvisoryError(t *testing.T) {
	exp := testExperiment()
	adv := evalkit.Score{Name: "fact_consistency", Pass: true, AdvisoryError: "SECRET_JUDGE_ERROR"}
	exp.Trials[0].Scores = append(exp.Trials[0].Scores, adv)
	exp.Trials = append(exp.Trials, evalkit.Trial{SampleID: "rice-ho-prot-9905", Suite: "holdout", Expected: model.OutcomeAdvice,
		Outcome: model.OutcomeAdvice, Card: &model.AdviceCard{}, Scores: []evalkit.Score{{Name: "outcome_match", Pass: true, Value: 1}, adv}})
	red := RedactHoldout(exp)
	if sc, _ := red.Trials[len(red.Trials)-1].ScoreOf("fact_consistency"); sc.AdvisoryError != RedactedText {
		t.Fatalf("holdout advisory error = %q, want %q", sc.AdvisoryError, RedactedText)
	}
	var b strings.Builder
	if err := Render(&b, red); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "judge_error（门禁未开") || !strings.Contains(out, "fact_consistency 2 次") {
		t.Fatalf("report missing judge_error count:\n%s", out)
	}
	if strings.Count(out, "SECRET_JUDGE_ERROR") != 1 {
		t.Fatalf("judge error text: want once (public trial detail only), got %d", strings.Count(out, "SECRET_JUDGE_ERROR"))
	}
}
