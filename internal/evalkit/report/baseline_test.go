package report

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"agri-steward/internal/evalkit"
)

// memPutter 是内存对象存储：记下每个键的内容类型与内容；failKey 命中时返回错误。
type memPutter struct {
	objects map[string][]byte
	types   map[string]string
	failKey string
}

func newMemPutter() *memPutter {
	return &memPutter{objects: map[string][]byte{}, types: map[string]string{}}
}

func (m *memPutter) PutObject(_ context.Context, bucket, key string, body []byte, ct string) error {
	if key == m.failKey {
		return errors.New("minio down")
	}
	m.objects[bucket+"/"+key], m.types[bucket+"/"+key] = body, ct
	return nil
}

func baselineExperiment() *evalkit.Experiment {
	exp := testExperiment()
	exp.Snapshot = evalkit.Snapshot{GitSHA: "1161f7e", PromptVersion: "p0-v1", ModelTier: "standard",
		Model: evalkit.ModelStub, KnowledgeRef: "WORKTREE", SamplesHash: "abc"}
	return exp
}

func TestPublishBaseline(t *testing.T) {
	const bucket = "agri-steward-eval"
	now := time.Date(2026, 10, 29, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	exp := baselineExperiment()
	root := t.TempDir()
	reportPath, err := WriteFile(root, exp)
	if err != nil {
		t.Fatal(err)
	}
	obj := newMemPutter()
	dir := filepath.Join(root, "baselines")
	p, b, err := PublishBaseline(context.Background(), obj, bucket, dir, "baseline_p0", exp, reportPath, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "baseline_p0.yaml"); p != want {
		t.Fatalf("path = %s, want %s", p, want)
	}
	reportKey := bucket + "/report/" + exp.ID + "/report.md"
	snapKey := bucket + "/report/" + exp.ID + "/snapshot.json"
	if !strings.HasPrefix(string(obj.objects[reportKey]), "# 评测报告") || obj.types[reportKey] != ReportContentType {
		t.Fatalf("report object missing or wrong type: %q", obj.types[reportKey])
	}
	var doc snapshotDoc
	if err := json.Unmarshal(obj.objects[snapKey], &doc); err != nil || doc.ExpID != exp.ID || doc.Snapshot != exp.Snapshot {
		t.Fatalf("snapshot object = %s (%v)", obj.objects[snapKey], err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got Baseline
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"exp_id", got.ExpID == exp.ID},
		{"model stub", got.Model == evalkit.ModelStub},
		{"keys", got.ReportKey == "report/"+exp.ID+"/report.md" && got.SnapshotKey == "report/"+exp.ID+"/snapshot.json"},
		{"snapshot", got.Snapshot == exp.Snapshot},
		{"generated_at utc", got.GeneratedAt.Equal(now) && got.GeneratedAt.Location() == time.UTC},
		{"counts", got.N == 2 && got.Samples == 2 && got.Trials == 4},
		{"gate", strings.HasPrefix(got.Gate, "门禁（Gate 0 自动项")},
		{"stub note", len(got.Notes) > 0 && strings.Contains(got.Notes[0], "真实模型未验证")},
		{"signoff pending", strings.Contains(strings.Join(got.Notes, "\n"), "待签字")},
		{"returned equals file", b.ExpID == got.ExpID && b.Gate == got.Gate},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: yaml =\n%s", c.name, raw)
		}
	}
	for _, field := range []string{"model: stub", "git_sha: 1161f7e", "prompt_version: p0-v1", "model_tier: standard", "knowledge_ref: WORKTREE"} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("yaml missing %q:\n%s", field, raw)
		}
	}
}

func TestPublishBaselineErrors(t *testing.T) {
	root := t.TempDir()
	exp := baselineExperiment()
	reportPath, err := WriteFile(root, exp)
	if err != nil {
		t.Fatal(err)
	}
	noSnap := baselineExperiment()
	noSnap.Snapshot = evalkit.Snapshot{}
	tests := []struct {
		name       string
		baseline   string
		exp        *evalkit.Experiment
		reportPath string
		failKey    string
	}{
		{name: "bad name", baseline: "../x", exp: exp, reportPath: reportPath},
		{name: "upper case name", baseline: "Baseline", exp: exp, reportPath: reportPath},
		{name: "missing snapshot", baseline: "b", exp: noSnap, reportPath: reportPath},
		{name: "missing report file", baseline: "b", exp: exp, reportPath: filepath.Join(root, "nope.md")},
		{name: "report upload fails", baseline: "b", exp: exp, reportPath: reportPath, failKey: ReportKey(exp.ID)},
		{name: "snapshot upload fails", baseline: "b", exp: exp, reportPath: reportPath, failKey: SnapshotKey(exp.ID)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newMemPutter()
			obj.failKey = tt.failKey
			dir := filepath.Join(t.TempDir(), "baselines")
			if _, _, err := PublishBaseline(context.Background(), obj, "agri-steward-eval", dir, tt.baseline, tt.exp,
				tt.reportPath, time.Unix(0, 0)); err == nil {
				t.Fatal("want error")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("baseline dir should not be written on error: %v", err)
			}
		})
	}
}

func TestNewBaselineRealModelNote(t *testing.T) {
	exp := baselineExperiment()
	exp.Snapshot.Model = "openai"
	b := NewBaseline("b", "bucket", exp, time.Unix(0, 0))
	if strings.Contains(strings.Join(b.Notes, "\n"), "真实模型未验证") {
		t.Fatalf("non-stub baseline must not carry stub note: %v", b.Notes)
	}
}

func TestRenderSnapshotSection(t *testing.T) {
	var b strings.Builder
	if err := Render(&b, baselineExperiment()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 配置快照", "| git_sha | `1161f7e` |", "| model | stub |", "真实模型未验证"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report missing %q", want)
		}
	}
	var plain strings.Builder
	if err := Render(&plain, testExperiment()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "## 配置快照") {
		t.Error("zero snapshot must not render section")
	}
}
