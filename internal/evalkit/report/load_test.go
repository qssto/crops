package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agri-steward/internal/evalkit"
	"agri-steward/internal/evalkit/reduce"
)

// loadExperiment 在 testExperiment 上给 s-1 两个 trial 挂加载观测（s-1#0 为 KB_MISS 拒答）。
func loadExperiment() *evalkit.Experiment {
	exp := testExperiment()
	exp.Trials[0].Load = &evalkit.LoadObs{QuestionType: "protect", Topic: "protect.timing", KBMiss: true, Strategies: []string{"v1"},
		MustCite: []string{"kb:a@v1", "kb:b@v1"}, Items: []evalkit.LoadedItem{{Ref: "kb:a@v1", PickReason: "RANKED"}}}
	exp.Trials[1].Load = &evalkit.LoadObs{QuestionType: "protect", Topic: "protect.timing", Strategies: []string{"v1"},
		MustCite: []string{"kb:a@v1", "kb:b@v1"}, Items: []evalkit.LoadedItem{
			{Ref: "kb:a@v1", PickReason: "RANKED", Cited: true}, {Ref: "kb:c@v1", PickReason: "COMMON", Irrelevant: true}}}
	return exp
}

func TestRenderLoad(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, loadExperiment()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"## 知识加载指标\n\n> 命中率 = must_cite",
		"策略：v1",
		"| 问题类型 | 样本 | trial | 加载命中率 | 加载精度 | 无关知识注入率（越低越好） |",
		"| protect | 1 | 2 | 50.00% (2 / 4) | 50.00% (1 / 2) | 33.33% (1 / 3) |",
		"| 合计 | 1 | 2 | 50.00% (2 / 4) | 50.00% (1 / 2) | 33.33% (1 / 3) |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// 加载节在 error 清单之后、按 tag 分组之前。
	if i, j := strings.Index(out, "## 知识加载指标"), strings.Index(out, "## 按 tag 分组"); i < 0 || j < i {
		t.Errorf("load section at %d, tag section at %d", i, j)
	}

	b.Reset()
	if err := Render(&b, testExperiment()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "## 知识加载指标\n\n无加载观测") {
		t.Errorf("no-observation note missing:\n%s", b.String())
	}
}

func TestWriteFile_LoadGaps(t *testing.T) {
	root := t.TempDir()
	if _, err := WriteFile(root, loadExperiment()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "20261028T010000-abcdef", LoadGapsFile))
	if err != nil {
		t.Fatal(err)
	}
	var g reduce.LoadGaps
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.KBMiss) != 1 || g.KBMiss[0].SampleID != "s-1" || len(g.LowHit) != 1 || g.LowHit[0].Topic != "protect.timing" ||
		len(g.LowHit[0].Missing) != 1 || g.LowHit[0].Missing[0] != "kb:b@v1" {
		t.Fatalf("gaps = %s", raw)
	}
	// 没有加载观测时不写清单；holdout 脱敏副本不带加载观测（redactTrial 只留白名单字段）。
	plain := t.TempDir()
	if _, err := WriteFile(plain, testExperiment()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plain, "20261028T010000-abcdef", LoadGapsFile)); !os.IsNotExist(err) {
		t.Fatalf("gaps file without observations: %v", err)
	}
	if redactTrial(loadExperiment().Trials[0]).Load != nil {
		t.Fatal("holdout redaction keeps load observation")
	}
}
