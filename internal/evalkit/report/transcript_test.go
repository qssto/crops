package report

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/model"
	"agri-steward/internal/repository"
	"agri-steward/internal/service"
)

func transcriptTrial() evalkit.Trial {
	q := &model.RunStartedPayload{Question: "稻飞虱要不要打药", FieldID: 3}
	return evalkit.Trial{SampleID: "s-1", TrialIndex: 2, RunID: 9, Expected: model.OutcomeAdvice,
		Outcome: model.OutcomeAdvice, Scope: model.Scope{TenantID: 1, FarmID: 1},
		Detail: &service.RunDetail{Run: model.RunSummary{RunID: 9}, Question: q,
			Timeline: []repository.EventRow{{Seq: 1, Type: model.EventRunStarted}, {Seq: 2, StepID: "enrich", Type: model.EventStepStarted}},
			Steps: []service.StepDetail{
				{StepRow: repository.StepRow{StepID: ""}, ToolCalls: []service.ToolCallDetail{},
					LLMCalls: []service.LLMCallDetail{}, EventSeqs: []int{1}},
				{StepRow: repository.StepRow{StepID: "enrich", Node: "enrich", Status: "COMPLETED"},
					ToolCalls: []service.ToolCallDetail{{ToolCallRow: repository.ToolCallRow{ID: 11, ToolName: "weather.forecast"}}},
					LLMCalls:  []service.LLMCallDetail{{EventSeq: 3}}, EventSeqs: []int{2}},
			}}}
}

func decodeLines(t *testing.T, b []byte) []TranscriptLine {
	t.Helper()
	var out []TranscriptLine
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var l TranscriptLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		out = append(out, l)
	}
	return out
}

func TestBuildTranscript_ThreeLayers(t *testing.T) {
	b, err := BuildTranscript("exp-1", transcriptTrial())
	if err != nil {
		t.Fatal(err)
	}
	lines := decodeLines(t, b)
	want := []struct{ layer, id, parent, kind string }{
		{LayerRoot, "run:9", "", ""},
		{LayerStep, "run:9/step:", "run:9", ""},
		{LayerAtomic, "run:9/step:/event:1", "run:9/step:", KindEvent},
		{LayerStep, "run:9/step:enrich", "run:9", ""},
		{LayerAtomic, "run:9/step:enrich/tool:11", "run:9/step:enrich", KindToolCall},
		{LayerAtomic, "run:9/step:enrich/llm:3", "run:9/step:enrich", KindLLMCall},
		{LayerAtomic, "run:9/step:enrich/event:2", "run:9/step:enrich", KindEvent},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %d, want %d:\n%s", len(lines), len(want), b)
	}
	for i, w := range want {
		l := lines[i]
		if l.Layer != w.layer || l.ID != w.id || l.ParentID != w.parent || l.Kind != w.kind {
			t.Errorf("line %d = %+v, want %+v", i, l, w)
		}
	}
	var root struct {
		ExpID    string `json:"exp_id"`
		SampleID string `json:"sample_id"`
		Trial    int    `json:"trial"`
		RunID    uint64 `json:"run_id"`
		Status   string `json:"status"`
		Question struct {
			Question string `json:"question"`
		} `json:"question"`
	}
	if err := json.Unmarshal(lines[0].Data, &root); err != nil {
		t.Fatal(err)
	}
	if root.ExpID != "exp-1" || root.SampleID != "s-1" || root.Trial != 2 || root.RunID != 9 ||
		root.Question.Question != "稻飞虱要不要打药" {
		t.Errorf("root = %+v", root)
	}
	var ev repository.EventRow
	if err := json.Unmarshal(lines[6].Data, &ev); err != nil || ev.Seq != 2 || ev.Type != model.EventStepStarted {
		t.Errorf("event line = %s (%v)", lines[6].Data, err)
	}
}

func TestBuildTranscript_NoDetail(t *testing.T) {
	b, err := BuildTranscript("exp-1", evalkit.Trial{SampleID: "s-1"})
	if err != nil || b != nil {
		t.Fatalf("b=%q err=%v", b, err)
	}
}

type fakePutter struct {
	keys   []string
	bucket string
	err    error
}

func (f *fakePutter) PutObject(_ context.Context, bucket, key string, body []byte, ct string) error {
	if f.err != nil {
		return f.err
	}
	if ct != TranscriptContentType || len(body) == 0 {
		return errors.New("bad upload")
	}
	f.bucket, f.keys = bucket, append(f.keys, key)
	return nil
}

func TestUploadTranscripts(t *testing.T) {
	exp := &evalkit.Experiment{ID: "exp-1", Trials: []evalkit.Trial{transcriptTrial(), {SampleID: "s-2"}}}
	p := &fakePutter{}
	n, err := UploadTranscripts(context.Background(), p, "agri-steward-eval", exp)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	const key = "transcript/exp-1/s-1/2.jsonl"
	if p.bucket != "agri-steward-eval" || len(p.keys) != 1 || p.keys[0] != key {
		t.Errorf("uploaded %s %v", p.bucket, p.keys)
	}
	if exp.Trials[0].Transcript != key || exp.Trials[1].Transcript != "" {
		t.Errorf("trial keys %q %q", exp.Trials[0].Transcript, exp.Trials[1].Transcript)
	}
	exp.Trials[0].Transcript = ""
	if _, err := UploadTranscripts(context.Background(), &fakePutter{err: errors.New("down")}, "b", exp); err == nil ||
		exp.Trials[0].Transcript != "" {
		t.Errorf("err=%v key=%q", err, exp.Trials[0].Transcript)
	}
}
