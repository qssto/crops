package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"agri-steward/internal/evalkit"
)

// L2 评审 P1-2：路径标为 holdout 的 trial 写库时 suite 记 holdout（读回即按它判定），不信样本 suite 字段。
func TestTrialInsert_HoldoutTagWinsSuite(t *testing.T) {
	exp := &evalkit.Experiment{FinishedAt: time.Unix(0, 0).UTC(), Trials: []evalkit.Trial{
		{SampleID: "h", Suite: "regression", Holdout: true, Error: "x"}}}
	_, args, err := trialInsert(7, exp)
	if err != nil {
		t.Fatal(err)
	}
	if args[4] != "holdout" {
		t.Fatalf("suite column = %v, want holdout", args[4])
	}
	tr, err := trialRow{SampleID: "h", Suite: "holdout", Status: StatusInfraError}.trial()
	if err != nil || !tr.Holdout {
		t.Fatalf("read back Holdout = %v, %v", tr.Holdout, err)
	}
}

func TestTrialInsert(t *testing.T) {
	pass := []evalkit.Score{{Name: "outcome_match", Pass: true, Value: 1},
		{Name: "injection_ignored", RedLine: true, Pass: true, Value: 1, NotApplicable: true}}
	exp := &evalkit.Experiment{FinishedAt: time.Unix(0, 0).UTC(), Trials: []evalkit.Trial{
		{SampleID: "a", SampleVersion: 3, Suite: "regression", RunID: 42, Scores: pass},
		{SampleID: "a", Suite: "regression", TrialIndex: 1, Error: "x"},
		{SampleID: "a", Suite: "regression", TrialIndex: 2, Scores: []evalkit.Score{
			{Name: "schema_valid", RedLine: true, Error: "grader schema_valid on a#2: boom"}}},
	}}
	q, args, err := trialInsert(7, exp)
	if err != nil {
		t.Fatal(err)
	}
	const cols = trialColCount
	if strings.Count(q, "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)") != len(exp.Trials) || strings.Count(q, "?") != len(args) {
		t.Fatalf("query/args mismatch: %s (%d args)", q, len(args))
	}
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"sample_version", args[2], 3},
		{"run_id", args[5], uint64(42)},
		{"status pass", args[6], StatusPass},
		{"passed", args[7], true},
		{"scores", args[8], `{"injection_ignored":{"pass":true,"value":1,"red_line":true,"not_applicable":true},` +
			`"outcome_match":{"pass":true,"value":1}}`},
		{"infra run_id null", args[cols+5], nil},
		{"infra status", args[cols+6], StatusInfraError},
		{"infra scores", args[cols+8], `{}`},
		{"grader error status", args[2*cols+6], StatusGraderError},
		{"grader error not passed", args[2*cols+7], false},
		{"grader error scores", args[2*cols+8],
			`{"schema_valid":{"pass":false,"value":0,"red_line":true,"error":"grader schema_valid on a#2: boom"}}`},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %#v, want %#v", tt.name, tt.got, tt.want)
		}
	}
}

// fullSnapshot 是字段齐全的快照（git_sha 40 位）。
func fullSnapshot() evalkit.Snapshot {
	return evalkit.Snapshot{GitSHA: strings.Repeat("a", gitSHALen), PromptVersion: "v1", ModelTier: "standard",
		Model: evalkit.ModelStub, KnowledgeRef: evalkit.KnowledgeContentPrefix + strings.Repeat("c", 64), SamplesHash: "h",
		LoaderStrategy: evalkit.LoaderV0, RetrievalStrategy: evalkit.RetrievalProgressive, JudgeVersions: map[string]string{},
		Suites: []string{"regression"}, Tags: []string{}, N: 5}
}

// TestStart_Rejects 校验 Start 在连库前拒绝空快照、缺字段快照、非法 trigger 与短 git_sha（db 为 nil，走到 SQL 即 panic）。
func TestStart_Rejects(t *testing.T) {
	noJudge := fullSnapshot()
	noJudge.JudgeVersions = nil
	shortSHA := fullSnapshot()
	shortSHA.GitSHA = "abc"
	noLoader := fullSnapshot()
	noLoader.LoaderStrategy = ""
	tests := []struct {
		name    string
		trigger string
		snap    evalkit.Snapshot
		want    error
	}{
		{"empty snapshot", TriggerManual, evalkit.Snapshot{}, ErrEmptySnapshot},
		{"missing loader_strategy", TriggerManual, noLoader, ErrEmptySnapshot},
		{"nil judge_versions", TriggerPR, noJudge, ErrEmptySnapshot},
		{"bad git_sha", TriggerNightly, shortSHA, ErrBadGitSHA},
		{"bad trigger", "cron", fullSnapshot(), ErrBadTrigger},
		{"empty trigger", "", fullSnapshot(), ErrBadTrigger},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(nil).Start(t.Context(), tt.trigger, tt.snap, time.Unix(0, 0))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestFinish_RejectsIncompleteSnapshot(t *testing.T) {
	exp := &evalkit.Experiment{StartedAt: time.Unix(0, 0), FinishedAt: time.Unix(90, 0)}
	if err := New(nil).Finish(t.Context(), 1, exp); !errors.Is(err, ErrEmptySnapshot) {
		t.Fatalf("err = %v, want ErrEmptySnapshot", err)
	}
	if exp.Snapshot.WallTime != "1m30s" {
		t.Fatalf("wall_time = %q, want 1m30s", exp.Snapshot.WallTime)
	}
}

// TestGuardedUpdateSQL 校验 experiment 的 UPDATE 都带冻结守卫（P2 §6.1 ②）。
func TestGuardedUpdateSQL(t *testing.T) {
	if got := guardedUpdateSQL("report_key = ?"); got != "UPDATE experiment SET report_key = ? WHERE id = ? AND frozen = 0" {
		t.Fatalf("sql = %q", got)
	}
}

func TestVersionUpdate(t *testing.T) {
	q, args := versionUpdate(9, []string{"a", "b"}, map[string]int{"a": 1, "b": 2})
	want := "UPDATE trial SET sample_version = CASE sample_id WHEN ? THEN ? WHEN ? THEN ? END WHERE experiment_id = ? AND sample_version = 0"
	if q != want || len(args) != 5 || args[0] != "a" || args[1] != 1 || args[3] != 2 || args[4] != uint64(9) {
		t.Fatalf("q = %s args = %v", q, args)
	}
}

func TestRecordCompare_Bad(t *testing.T) {
	if err := New(nil).RecordCompare(t.Context(), 2, 1, "X", []byte(`[]`)); !errors.Is(err, ErrBadCompareMode) {
		t.Fatalf("err = %v, want ErrBadCompareMode", err)
	}
	if err := New(nil).RecordCompare(t.Context(), 2, 1, CompareFactor, []byte(`{`)); err == nil {
		t.Fatal("invalid json must be rejected")
	}
}

func TestSetReportKey_Bad(t *testing.T) {
	for _, key := range []string{"", strings.Repeat("k", reportKeyMax+1)} {
		if err := New(nil).SetReportKey(t.Context(), 1, key); !errors.Is(err, ErrBadReportKey) {
			t.Fatalf("key len %d: err = %v, want ErrBadReportKey", len(key), err)
		}
	}
}

// L2 评审裁决：AdvisoryError 写进 trial.scores 并读回，状态仍按代码打分器（PASS）。
func TestTrialInsert_AdvisoryErrorRoundTrip(t *testing.T) {
	exp := &evalkit.Experiment{FinishedAt: time.Unix(0, 0).UTC(), Trials: []evalkit.Trial{{SampleID: "a", Suite: "regression",
		Scores: []evalkit.Score{{Name: "outcome_match", Pass: true, Value: 1}, {Name: "fact_consistency", Pass: true, AdvisoryError: "bad evidence"}}}}}
	_, args, err := trialInsert(7, exp)
	if err != nil {
		t.Fatal(err)
	}
	if args[6] != StatusPass || args[7] != true {
		t.Fatalf("status = %v passed = %v, want PASS", args[6], args[7])
	}
	scores := args[8].(string)
	tr, err := trialRow{SampleID: "a", Suite: "regression", Status: StatusPass, Scores: &scores}.trial()
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := tr.ScoreOf("fact_consistency"); sc.AdvisoryError != "bad evidence" || sc.Error != "" || tr.Status() != StatusPass {
		t.Fatalf("read back %+v status %s", sc, tr.Status())
	}
}
