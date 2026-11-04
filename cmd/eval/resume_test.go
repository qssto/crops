package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/store"
	"agri-steward/internal/model"
	"agri-steward/internal/service"
)

func resumePool() []*sample.Sample {
	q := "flaky"
	return []*sample.Sample{{ID: "a", Version: 1, Suite: "regression"}, {ID: "b", Version: 2, Suite: "regression"},
		{ID: "q", Version: 1, Suite: "regression", Quarantine: &q}}
}

// 清单里的 sample_id@version 必须全部在当前样本集中，否则拒绝（失败关闭）。
func TestPickManifest(t *testing.T) {
	got, err := pickManifest(resumePool(), []string{"a@1", "q@1"})
	if err != nil || len(got) != 2 || got[0].ID != "a" || got[1].ID != "q" {
		t.Fatalf("got %v err %v", got, err)
	}
	for _, refs := range [][]string{{"a@2"}, {"a@1", "zz@1"}, nil} {
		if _, err := pickManifest(resumePool(), refs); !errors.Is(err, errResumeSamples) {
			t.Fatalf("%v: err = %v", refs, err)
		}
	}
}

// 补跑键 = 缺失 + INFRA_ERROR 的 (sample_id, trial_index)；隔离样本不补。
func TestResumeKeys(t *testing.T) {
	trials := []evalkit.Trial{
		{SampleID: "a", TrialIndex: 0, Scores: []evalkit.Score{{Name: "x", Pass: true}}},
		{SampleID: "a", TrialIndex: 1, Error: "infra"},
		{SampleID: "b", TrialIndex: 0, Scores: []evalkit.Score{{Name: "x", Error: "grader"}}},
	}
	got := resumeKeys(resumePool(), 2, trials)
	want := []evalkit.TrialKey{{SampleID: "a", Index: 1}, {SampleID: "b", Index: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

// 因子或 samples_hash 不一致拒绝 resume；运行结果字段（token_used / wall_time）与 git 不算。
func TestCheckResumeSnapshot(t *testing.T) {
	base := evalkit.Snapshot{PromptVersion: "p1", ModelTier: "t", Model: "stub", KnowledgeRef: "kc:1",
		LoaderStrategy: "v0", RetrievalStrategy: "prog", SamplesHash: "h", JudgeVersions: map[string]string{},
		VoidInfraErrorRate: 0.05}
	same := base
	// 当前配置的作废阈值不参与比较：resume 一律沿用快照里的值。
	same.GitSHA, same.TokenUsed, same.WallTime, same.VoidInfraErrorRate = "other", 9, "1s", 0.01
	if err := checkResumeSnapshot(base, same, "h"); err != nil {
		t.Fatalf("same: %v", err)
	}
	prompt := base
	prompt.PromptVersion = "p2"
	kb := base
	kb.KnowledgeRef = "kc:2"
	samples := base
	samples.SamplesHash = "h2"
	for name, cur := range map[string]evalkit.Snapshot{"prompt": prompt, "knowledge": kb, "samples": samples} {
		if err := checkResumeSnapshot(base, cur, "h"); !errors.Is(err, errResumeSnapshot) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if err := checkResumeSnapshot(base, base, "other"); !errors.Is(err, errResumeSnapshot) {
		t.Fatalf("manifest hash: err = %v", err)
	}
	// 快照没记录作废阈值（L7-a 评审修复前的实验）或记录值越界：失败关闭。
	for _, rate := range []float64{0, 0.2} {
		bad := base
		bad.VoidInfraErrorRate = rate
		if err := checkResumeSnapshot(bad, bad, "h"); !errors.Is(err, errResumeSnapshot) {
			t.Fatalf("void rate %v: err = %v", rate, err)
		}
	}
}

// 合并：补跑结果覆盖同键旧行（INFRA_ERROR），其余保留；按样本顺序、序号排列，旧行补回样本 tags。
func TestMergeTrials(t *testing.T) {
	pool := resumePool()
	pool[0].Tags = []string{"L2"}
	kept := []evalkit.Trial{{SampleID: "b", TrialIndex: 0}, {SampleID: "a", TrialIndex: 1, Error: "infra"}, {SampleID: "a", TrialIndex: 0}}
	fresh := []evalkit.Trial{{SampleID: "a", TrialIndex: 1, RunID: 5}}
	got := mergeTrials(pool, kept, fresh)
	if len(got) != 3 || got[0].SampleID != "a" || got[0].TrialIndex != 0 || got[1].RunID != 5 || got[1].Error != "" ||
		got[2].SampleID != "b" || !slices.Equal(got[0].Tags, []string{"L2"}) {
		t.Fatalf("merged = %+v", got)
	}
}

// resumeLog 记录写库与上传的先后。
type resumeLog struct {
	calls []string
	rate  float64
	fail  error
}

func (l *resumeLog) Resume(_ context.Context, _ uint64, _ *evalkit.Experiment, _ bool, rate float64) (store.Settlement, error) {
	l.calls, l.rate = append(l.calls, "resume"), rate
	if l.fail != nil {
		return store.Settlement{}, l.fail
	}
	return store.Settlement{Status: "DONE"}, nil
}

func (l *resumeLog) PutObject(_ context.Context, _, key string, _ []byte, _ string) error {
	l.calls = append(l.calls, key)
	return nil
}

// 写库事务成功后才上传 transcript，且只传终态 trial（INFRA_ERROR 行可被下一次 resume 替换，不传）；
// 事务失败不上传；判作废用快照记录的阈值。
func TestCommitResume(t *testing.T) {
	detail := &service.RunDetail{Run: model.RunSummary{RunID: 9}}
	fresh := func() *evalkit.Experiment {
		return &evalkit.Experiment{ID: "7", Snapshot: evalkit.Snapshot{VoidInfraErrorRate: 0.03}, Trials: []evalkit.Trial{
			{SampleID: "a", TrialIndex: 1, RunID: 9, Detail: detail, Scores: []evalkit.Score{{Name: "x", Pass: true}}},
			{SampleID: "b", TrialIndex: 0, Detail: detail, Error: "infra"},
		}}
	}
	l := &resumeLog{fail: errors.New("boom")}
	if _, err := commitResume(context.Background(), l, l, "eval", 7, fresh()); !errors.Is(err, l.fail) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(l.calls, []string{"resume"}) {
		t.Fatalf("failed tx uploaded: %v", l.calls)
	}
	l = &resumeLog{}
	exp := fresh()
	if st, err := commitResume(context.Background(), l, l, "eval", 7, exp); err != nil || st.Status != "DONE" {
		t.Fatalf("st = %+v err = %v", st, err)
	}
	if want := []string{"resume", "transcript/7/a/1.jsonl"}; !slices.Equal(l.calls, want) || l.rate != 0.03 {
		t.Fatalf("calls = %v rate = %v, want %v 0.03", l.calls, l.rate, want)
	}
	if exp.Trials[0].Transcript != "transcript/7/a/1.jsonl" || exp.Trials[1].Transcript != "" {
		t.Fatalf("transcript keys = %q %q", exp.Trials[0].Transcript, exp.Trials[1].Transcript)
	}
}
