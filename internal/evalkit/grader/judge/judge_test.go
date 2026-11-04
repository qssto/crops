package judge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/knowledge"
	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/model"
)

// fakeKB 是带原文的知识版本视图。
type fakeKB map[string]string

func (k fakeKB) Has(id, v string) bool { _, ok := k[id+"@"+v]; return ok }

func (k fakeKB) Load(_ context.Context, id, v string) (agent.KnowledgeEntry, error) {
	body, ok := k[id+"@"+v]
	if !ok {
		return agent.KnowledgeEntry{}, knowledge.ErrNotFound
	}
	return agent.KnowledgeEntry{KBID: id, Version: v, Body: json.RawMessage(body)}, nil
}

func testTrial() *grader.Trial {
	s := &sample.Sample{ID: "rice-test-0001", Question: "稻飞虱要不要打药？", Crop: "rice",
		Context:  sample.Context{Farm: "f1", Field: "a1", Role: "farmer", User: "u1"},
		Fixtures: sample.Fixtures{Clock: "2026-07-01T08:00:00+08:00"},
		Expect: sample.Expect{Outcome: model.OutcomeAdvice,
			Advice: &sample.AdviceExpect{Category: "protect", Conclusion: "建议 3 天内防治稻飞虱"}}}
	return &grader.Trial{Sample: s, Outcome: model.OutcomeAdvice,
		Card: &model.AdviceCard{Conclusion: "建议 3 天内防治稻飞虱。", Reasons: []string{"百丛虫量已超过防治指标"},
			Citations: []model.Citation{{KBID: "kb-1", KBVersion: "v1"}, {KBID: "kb-1", KBVersion: "v1"}, {KBID: "kb-2", KBVersion: "v9"}}},
		KB: fakeKB{"kb-1@v1": `{"body":"百丛 1000 头以上达防治指标"}`},
		// 中间步骤：不得进材料。
		ToolCalls: map[string]int{"weather.forecast": 1}, Logs: []byte(`{"msg":"reasoning step"}`),
		ToolArgs: []grader.ToolArg{{Tool: "weather.forecast", Args: []byte(`{"secret_step":1}`)}},
		Events:   []grader.RunEvent{{Type: "LLM_CALL", Payload: json.RawMessage(`{"thought":"中间推理"}`)}},
		Stored:   []grader.StoredText{{Source: "tool_call.result_summary#1", Text: "中间结果"}}}
}

func newJudges(t *testing.T, recs ...Recording) (*Replay, map[string]*Judge) {
	t.Helper()
	r := &Replay{recs: map[recKey]Verdict{}}
	for _, rec := range recs {
		if err := r.Add(rec.Dimension, rec); err != nil {
			t.Fatal(err)
		}
	}
	js := map[string]*Judge{}
	for _, d := range Dimensions {
		j, err := New(filepath.Join("..", "..", "..", "..", DefaultDir), d, r)
		if err != nil {
			t.Fatal(err)
		}
		js[d] = j
	}
	return r, js
}

func score(n int) *int { return &n }

// 验收 1：每个 judge 表驱动覆盖 1/2/3 分与 UNKNOWN（录制回放），evidence 非原句与越界分数记 grader_error。
func TestJudge_Replay(t *testing.T) {
	tr := testTrial()
	tests := []struct {
		name        string
		rec         *Verdict // nil：不录制
		wantValue   float64
		wantUnknown bool
		wantErr     error
	}{
		{name: "score 3", rec: &Verdict{Score: score(3), Evidence: "建议 3 天内防治稻飞虱", Verdict: "一致"}, wantValue: 3},
		{name: "score 2", rec: &Verdict{Score: score(2), Evidence: "百丛虫量已超过防治指标", Verdict: "部分"}, wantValue: 2},
		{name: "score 1 punctuation folded", rec: &Verdict{Score: score(1), Evidence: "建议3天内，防治稻飞虱！", Verdict: "差"}, wantValue: 1},
		{name: "unknown verdict", rec: &Verdict{Verdict: "材料不足"}, wantUnknown: true},
		{name: "no recording", wantUnknown: true},
		{name: "evidence not quoted", rec: &Verdict{Score: score(3), Evidence: "建议立即喷药", Verdict: "x"}, wantErr: ErrEvidenceNotQuoted},
		{name: "evidence from knowledge", rec: &Verdict{Score: score(3), Evidence: "百丛 1000 头以上达防治指标"}, wantErr: ErrEvidenceNotQuoted},
		{name: "empty evidence", rec: &Verdict{Score: score(2)}, wantErr: ErrEvidenceNotQuoted},
	}
	for _, dim := range Dimensions {
		for _, tt := range tests {
			t.Run(dim+"/"+tt.name, func(t *testing.T) {
				m, err := BuildMaterial(context.Background(), dim, tr)
				if err != nil {
					t.Fatal(err)
				}
				h, err := m.Hash()
				if err != nil {
					t.Fatal(err)
				}
				var recs []Recording
				if tt.rec != nil {
					recs = append(recs, Recording{Dimension: dim, MaterialHash: h, Verdict: *tt.rec})
				}
				_, js := newJudges(t, recs...)
				sc, err := js[dim].Grade(context.Background(), tr)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if err != nil {
					return
				}
				if !sc.Pass || sc.Unknown != tt.wantUnknown || sc.Value != tt.wantValue {
					t.Fatalf("score = %+v, want value %v unknown %v (judge 只进报告，Pass 恒 true)", sc, tt.wantValue, tt.wantUnknown)
				}
			})
		}
	}
}

// L2 评审 P2-6：evidence 归一后至少 MinEvidenceRunes 个字、不跨句（句末标点只许在结尾），过短或拼接两句记 grader_error。
func TestToScore_EvidenceShape(t *testing.T) {
	evaluated := "建议 3 天内防治稻飞虱。\n百丛虫量已超过防治指标\n"
	tests := []struct {
		name, evidence string
		wantErr        error
	}{
		{name: "whole sentence with end mark", evidence: "建议 3 天内防治稻飞虱。"},
		{name: "exactly min runes", evidence: "量已超过防治指标"},
		{name: "too short", evidence: "稻飞虱", wantErr: ErrEvidenceTooShort},
		{name: "too short after fold", evidence: "防治，稻飞虱！", wantErr: ErrEvidenceTooShort},
		{name: "spans two sentences", evidence: "防治稻飞虱。百丛虫量已超过", wantErr: ErrEvidenceCrossSentence},
		{name: "spans newline", evidence: "防治稻飞虱\n百丛虫量已超过", wantErr: ErrEvidenceCrossSentence},
		{name: "semicolon inside", evidence: "防治稻飞虱；百丛虫量已超过", wantErr: ErrEvidenceCrossSentence},
		{name: "ascii question inside", evidence: "防治稻飞虱?百丛虫量已超过", wantErr: ErrEvidenceCrossSentence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ToScore(Verdict{Score: score(2), Evidence: tt.evidence, Verdict: "x"}, evaluated)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// L2 评审裁决：门禁未开的维度 Advisory（出错不改 trial 状态），开了的维度出错照旧 grader_error。
func TestAll_AdvisoryByGate(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", DefaultDir)
	for name, gated := range map[string]func(string) bool{
		"nil gate": nil,
		"fact on":  func(d string) bool { return d == DimFactConsistency },
	} {
		gs, err := All(dir, &Replay{recs: map[recKey]Verdict{}}, gated)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range gs {
			adv, ok := g.(grader.Advisory)
			want := gated == nil || !gated(g.Name())
			if !ok || adv.Advisory() != want {
				t.Errorf("%s: %s advisory = %v, want %v", name, g.Name(), ok && adv.Advisory(), want)
			}
		}
	}
}

// 录制按维度区分：fact_consistency 的录制不被 conclusion_match 命中。
func TestReplay_KeyedByDimension(t *testing.T) {
	tr := testTrial()
	m, err := BuildMaterial(context.Background(), DimFactConsistency, tr)
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.Hash()
	if err != nil {
		t.Fatal(err)
	}
	_, js := newJudges(t, Recording{Dimension: DimFactConsistency, MaterialHash: h, Verdict: Verdict{Score: score(3), Evidence: "建议 3 天内防治稻飞虱"}})
	sc, err := js[DimConclusionMatch].Grade(context.Background(), tr)
	if err != nil || !sc.Unknown {
		t.Fatalf("conclusion_match = %+v, %v; want UNKNOWN", sc, err)
	}
}

func TestReplay_AddRejects(t *testing.T) {
	r := &Replay{recs: map[recKey]Verdict{}}
	ok := Recording{Dimension: DimFactConsistency, MaterialHash: "h", Verdict: Verdict{Score: score(2)}}
	if err := r.Add(DimFactConsistency, ok); err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]Recording{
		"duplicate":   ok,
		"wrong dim":   {Dimension: DimConclusionMatch, MaterialHash: "x"},
		"empty hash":  {Dimension: DimFactConsistency},
		"score range": {Dimension: DimFactConsistency, MaterialHash: "y", Verdict: Verdict{Score: score(4)}},
	} {
		if err := r.Add(DimFactConsistency, rec); !errors.Is(err, ErrBadRecording) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestLoadReplay_Files(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, DimConclusionMatch, RecordingsDir, "a.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"dimension":"conclusion_match","material_hash":"abc","score":2,"evidence":"e","verdict":"v"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := LoadReplay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.recs[recKey{DimConclusionMatch, "abc"}]; v.Score == nil || *v.Score != 2 {
		t.Fatalf("recording = %+v", v)
	}
	if err := os.WriteFile(p, []byte(`{"dimension":"fact_consistency","material_hash":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReplay(dir); !errors.Is(err, ErrBadRecording) {
		t.Fatalf("misplaced recording err = %v", err)
	}
}

// 材料只有 fixture、所引知识原文、专家标准结论、最终输出：不含工具调用、日志、事件、落库摘要等中间步骤。
func TestBuildMaterial_NoIntermediateSteps(t *testing.T) {
	m, err := BuildMaterial(context.Background(), DimFactConsistency, testTrial())
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"tool_calls", "weather.forecast", "reasoning", "secret_step", "thought", "中间推理", "中间结果", "events", "logs", "transcript"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("material contains intermediate step %q: %s", bad, b)
		}
	}
	top := reflect.TypeOf(m)
	var names []string
	for i := range top.NumField() {
		names = append(names, top.Field(i).Tag.Get("json"))
	}
	if got, want := strings.Join(names, ","), "dimension,fixture,knowledge,expected,output"; got != want {
		t.Fatalf("material fields = %s, want %s", got, want)
	}
	// 所引知识：同版本去重，不存在的版本记 missing。
	if len(m.Knowledge) != 2 || !strings.Contains(string(m.Knowledge[0].Body), "防治指标") || !m.Knowledge[1].Missing {
		t.Fatalf("knowledge = %+v", m.Knowledge)
	}
	if m.Expected.Conclusion != "建议 3 天内防治稻飞虱" || m.Output.Reasons[0] != "百丛虫量已超过防治指标" {
		t.Fatalf("material = %+v", m)
	}
}

func TestBuildMaterial_NoKnowledge(t *testing.T) {
	tr := testTrial()
	tr.KB = nil
	if _, err := BuildMaterial(context.Background(), DimFactConsistency, tr); !errors.Is(err, ErrNoKnowledge) {
		t.Fatalf("err = %v", err)
	}
	tr.Card = nil
	if _, err := BuildMaterial(context.Background(), DimFactConsistency, tr); err != nil {
		t.Fatalf("refusal without card: %v", err)
	}
}

// judge_version = 提示词 + 评分标准内容哈希 + 档位：任一变更版本即变。
func TestVersion_ChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{PromptFile, RubricFile} {
		p := filepath.Join(dir, DimFactConsistency, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	v1, err := Version(dir, DimFactConsistency, "t1")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Version(dir, DimFactConsistency, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DimFactConsistency, RubricFile), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	v3, err := Version(dir, DimFactConsistency, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 || v1 == v3 || !strings.HasSuffix(v1, "-t1") || len(v1) > versionLen {
		t.Fatalf("versions %s %s %s", v1, v2, v3)
	}
	if _, err := Version(dir, "nope", "t1"); !errors.Is(err, ErrUnknownDim) {
		t.Fatalf("unknown dim err = %v", err)
	}
	if _, err := Version(dir, DimFactConsistency, strings.Repeat("x", 20)); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("long tier err = %v", err)
	}
}

// 仓库内 eval/judges：配置可读、三个维度都有提示词与评分标准。
func TestRepoJudges(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	c, err := LoadConfig(filepath.Join(root, DefaultConfig))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := Versions(filepath.Join(root, DefaultDir), c.ModelTier)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != len(Dimensions) {
		t.Fatalf("versions = %v", vs)
	}
	if _, err := LoadReplay(filepath.Join(root, DefaultDir)); err != nil {
		t.Fatal(err)
	}
}
