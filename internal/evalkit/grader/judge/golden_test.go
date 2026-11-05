package judge

import (
	"context"
	"path/filepath"
	"testing"

	"agri-steward/internal/evalkit/grader"
	"agri-steward/internal/evalkit/sample"
	"agri-steward/internal/evalkit/textnorm"
	qjudge "agri-steward/internal/service/quality/judge"
)

// P2-L3 下沉 judge 公共部分前录下的金标：材料哈希（录制回放的键）与 judge_versions 必须不变。
var goldenHashes = map[string]string{
	DimFactConsistency:    "1630c9a3a67f640e293b49e15731d525d2e47b021efca4f12bfc4becd8843d85",
	DimConclusionMatch:    "9948181d500b349a6d53dddafec2e12cf3bb4979e6b96c9a1735023281062b9b",
	DimExplanationQuality: "3e3699b141b36eb2e4c96d1c5f0305c1128ce53729498e73046a60db23f0c07d",
	"no_card":             "ec6e4c5605e419449e6c405051a698100584a15d9726e3ecb97f9c3fd83cabf9",
	"rich":                "a88bdf893408fae5880b8b37f1cad337e99d21ff30928824f5818f6524bc6cb9",
}

var goldenVersions = map[string]string{
	DimFactConsistency:    "87ac7c8a4e2a51d3-replay-v0",
	DimConclusionMatch:    "27b399d545f7efbd-replay-v0",
	DimExplanationQuality: "d828e811fb9e3bc9-replay-v0",
}

// richTrial 在 testTrial 上加 MCP fixture（含故障与缓存）、记忆与 HTML 特殊字符，覆盖 fixture 序列化的各分支。
func richTrial() *grader.Trial {
	tr := testTrial()
	s := *tr.Sample
	s.Stage = "tillering"
	s.Fixtures.MCP = map[string][]sample.ToolFixture{
		"weather.forecast": {{Match: map[string]any{"days": 3}, Return: map[string]any{"rain_mm": 1.5, "note": "<a&b>"}}},
		"field.pest":       {{Error: "timeout", Cache: map[string]any{"collected_at": "2026-06-30T08:00:00+08:00"}}},
	}
	s.Fixtures.Memory = map[string]any{"z": []any{1, "二"}, "a": map[string]any{"k": true}}
	tr.Sample = &s
	return tr
}

func materialHash(t *testing.T, dim string, tr *grader.Trial) string {
	t.Helper()
	m, err := BuildMaterial(context.Background(), dim, tr)
	if err != nil {
		t.Fatal(err)
	}
	h, err := m.Hash()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestGolden_MaterialHashAndVersions(t *testing.T) {
	for _, s := range []string{"建议3天内，防治稻飞虱！", "ＡＢＣ　１２３ x-y_z", "２０２６年 7.5℃", ""} {
		if got, want := qjudge.Fold(s), textnorm.Fold(s); got != want {
			t.Errorf("Fold(%q) = %q, textnorm %q", s, got, want)
		}
	}
	for _, d := range Dimensions {
		if got := materialHash(t, d, testTrial()); got != goldenHashes[d] {
			t.Errorf("%s hash = %s, want %s", d, got, goldenHashes[d])
		}
	}
	noCard := testTrial()
	noCard.Card = nil
	if got := materialHash(t, DimFactConsistency, noCard); got != goldenHashes["no_card"] {
		t.Errorf("no-card hash = %s, want %s", got, goldenHashes["no_card"])
	}
	if got := materialHash(t, DimFactConsistency, richTrial()); got != goldenHashes["rich"] {
		t.Errorf("rich hash = %s, want %s", got, goldenHashes["rich"])
	}
	root := filepath.Join("..", "..", "..", "..")
	c, err := LoadConfig(filepath.Join(root, DefaultConfig))
	if err != nil {
		t.Fatal(err)
	}
	vs, err := Versions(filepath.Join(root, DefaultDir), c.ModelTier)
	if err != nil {
		t.Fatal(err)
	}
	for d, want := range goldenVersions {
		if vs[d] != want {
			t.Errorf("%s version = %s, want %s", d, vs[d], want)
		}
	}
}
