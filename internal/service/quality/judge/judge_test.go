package judge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func intp(n int) *int { return &n }

func TestRecordings_AddAndJudge(t *testing.T) {
	m := Material{Dimension: DimFactConsistency, Fixture: []byte(`{"question":"q"}`)}
	h, err := m.Hash()
	if err != nil {
		t.Fatal(err)
	}
	r := Recordings{}
	if err := r.Add(DimFactConsistency, Recording{Dimension: DimFactConsistency, MaterialHash: h, Verdict: Verdict{Score: intp(2)}}); err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]Recording{
		"duplicate":   {Dimension: DimFactConsistency, MaterialHash: h},
		"wrong dim":   {Dimension: DimConclusionMatch, MaterialHash: "x"},
		"empty hash":  {Dimension: DimFactConsistency},
		"score range": {Dimension: DimFactConsistency, MaterialHash: "y", Verdict: Verdict{Score: intp(0)}},
	} {
		if err := r.Add(DimFactConsistency, rec); !errors.Is(err, ErrBadRecording) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	v, err := r.Judge(context.Background(), Prompt{Dimension: DimFactConsistency}, m)
	if err != nil || v.Unknown() || *v.Score != 2 {
		t.Fatalf("hit = %+v, %v", v, err)
	}
	v, err = r.Judge(context.Background(), Prompt{Dimension: DimConclusionMatch}, m)
	if err != nil || !v.Unknown() {
		t.Fatalf("miss = %+v, %v; want UNKNOWN", v, err)
	}
}

func TestValidate(t *testing.T) {
	evaluated := "建议 3 天内防治稻飞虱。\n百丛虫量已超过防治指标"
	tests := []struct {
		name string
		v    Verdict
		want error
	}{
		{"unknown skips checks", Verdict{}, nil},
		{"ok", Verdict{Score: intp(3), Evidence: "建议 3 天内防治稻飞虱"}, nil},
		{"score low", Verdict{Score: intp(0), Evidence: "建议 3 天内防治稻飞虱"}, ErrBadScore},
		{"score high", Verdict{Score: intp(4), Evidence: "建议 3 天内防治稻飞虱"}, ErrBadScore},
		{"not quoted", Verdict{Score: intp(1), Evidence: "建议立即喷施吡虫啉药剂"}, ErrEvidenceNotQuoted},
		{"too short", Verdict{Score: intp(1), Evidence: "稻飞虱"}, ErrEvidenceTooShort},
		{"cross sentence", Verdict{Score: intp(1), Evidence: "防治稻飞虱。百丛虫量已超过"}, ErrEvidenceCrossSentence},
	}
	for _, tt := range tests {
		if err := Validate(tt.v, evaluated); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestLoadPromptAndVersion(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{PromptFile, RubricFile} {
		p := filepath.Join(dir, DimConclusionMatch, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f+" body"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := LoadPrompt(dir, DimConclusionMatch)
	if err != nil || p.Prompt != PromptFile+" body" || p.Rubric != RubricFile+" body" {
		t.Fatalf("prompt = %+v, %v", p, err)
	}
	if _, err := LoadPrompt(dir, DimFactConsistency); err == nil {
		t.Fatal("missing prompt files must fail")
	}
	if _, err := LoadPrompt(dir, "nope"); !errors.Is(err, ErrUnknownDim) {
		t.Fatalf("unknown dim err = %v", err)
	}
	v, err := Version(dir, DimConclusionMatch, "t1")
	if err != nil || len(v) != 16+len("-t1") {
		t.Fatalf("version = %q, %v", v, err)
	}
}

func TestMaterial_SetCardNoKnowledge(t *testing.T) {
	var m Material
	if err := m.SetCard(context.Background(), nil, nil); err != nil {
		t.Fatalf("nil card: %v", err)
	}
}
